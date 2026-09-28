package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/moderatorkey"
)

const moderatorKeysUsage = `usage:
  lens moderator-keys                     list every moderator key
  lens moderator-keys create <name>       create one; the key is printed ONCE
  lens moderator-keys revoke <id>         stop key <id> working, from the next request
  lens moderator-keys uses <id>           the last 50 uses of key <id>
See docs/moderator-keys.md.`

// moderatorKeyAdmin is the slice of *moderatorkey.Store the command needs.
type moderatorKeyAdmin interface {
	Create(ctx context.Context, name, by string) (string, *moderatorkey.Key, error)
	Revoke(ctx context.Context, id int64, by string) error
	List(ctx context.Context) ([]moderatorkey.Key, error)
	Uses(ctx context.Context, keyID int64, limit int) ([]moderatorkey.Use, error)
}

// runModeratorKeys is `lens moderator-keys` (B20.13), run inside the lens container so it reaches the
// same Postgres the server reads.
func runModeratorKeys(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("moderator-keys: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("moderator-keys: database: %w", err)
	}
	defer pool.Close()
	by := "operator-cli"
	if u := os.Getenv("USER"); u != "" {
		by += ":" + u
	}
	return moderatorKeysCommand(ctx, moderatorkey.NewStore(pool), args, by, os.Stdout)
}

func moderatorKeysCommand(ctx context.Context, store moderatorKeyAdmin, args []string, by string, out io.Writer) error {
	switch {
	case len(args) == 0:
		keys, err := store.List(ctx)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			fmt.Fprintln(out, "no moderator keys")
		}
		for _, k := range keys {
			state := "active"
			if k.RevokedAt != nil {
				state = "revoked " + k.RevokedAt.UTC().Format(time.RFC3339) + " by " + k.RevokedBy
			}
			fmt.Fprintf(out, "%d\t%s…\t%s\tcreated %s by %s\t%s\n",
				k.ID, k.KeyPrefix, k.Name, k.CreatedAt.UTC().Format(time.RFC3339), k.CreatedBy, state)
		}
		return nil
	case args[0] == "create" && len(args) >= 2:
		raw, k, err := store.Create(ctx, strings.Join(args[1:], " "), by)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "created moderator key %d (%s)\n\n  %s\n\n", k.ID, k.Name, raw)
		fmt.Fprintln(out, "This is the only time it is shown; Lens keeps only its hash.")
		fmt.Fprintln(out, "Put it in /etc/talyvor/bff.env as LENS_MODERATOR_KEY=<key> and restart the web app.")
		return nil
	case (args[0] == "revoke" || args[0] == "uses") && len(args) == 2:
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("moderator-keys: %q is not a key id\n%s", args[1], moderatorKeysUsage)
		}
		if args[0] == "revoke" {
			if err := store.Revoke(ctx, id, by); err != nil {
				return err
			}
			fmt.Fprintf(out, "revoked moderator key %d: it answers 401 from the next request\n", id)
			return nil
		}
		uses, err := store.Uses(ctx, id, 50)
		if err != nil {
			return err
		}
		if len(uses) == 0 {
			fmt.Fprintln(out, "no uses")
		}
		for _, u := range uses {
			fmt.Fprintf(out, "%s\t%s\t%s %s\n", u.CreatedAt.UTC().Format(time.RFC3339), u.Operator, u.Method, u.Path)
		}
		return nil
	}
	return fmt.Errorf("%s", moderatorKeysUsage)
}
