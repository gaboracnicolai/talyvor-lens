package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/economy"
)

const escrowsUsage = `usage:
  lens escrows                             every disputed escrow waiting on a decision, oldest first
  lens escrows release <escrow-id> <note>  decide it for the payee: the credits go to the payee's agent
  lens escrows return <escrow-id> <note>   decide it for the payer: the credits go back to the payer's agent
See docs/escrow.md.`

// escrowAdmin is the slice of *economy.DualTokenStore the command needs.
type escrowAdmin interface {
	ListDisputedEscrows(ctx context.Context) ([]economy.Escrow, error)
	DecideEscrow(ctx context.Context, escrowID string, release bool, operator, note string) (economy.Escrow, error)
}

// runEscrows is `lens escrows` (B22.6), run inside the lens container so it reaches the same Postgres the
// server reads.
func runEscrows(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("escrows: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("escrows: database: %w", err)
	}
	defer pool.Close()
	by := "operator-cli"
	if u := os.Getenv("USER"); u != "" {
		by += ":" + u
	}
	return escrowsCommand(ctx, economy.NewDualTokenStore(nil, pool, nil), args, by, os.Stdout)
}

func escrowsCommand(ctx context.Context, store escrowAdmin, args []string, by string, out io.Writer) error {
	switch {
	case len(args) == 0:
		list, err := store.ListDisputedEscrows(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintln(out, "no disputed escrows")
		}
		for _, e := range list {
			reason := ""
			for _, ev := range e.Events {
				if ev.Kind == "disputed" {
					reason = ev.Detail
				}
			}
			fmt.Fprintf(out, "%s\t%d µLXC\t%s\tfrom %s (%s) to %s (%s)\t%q\tdisputed: %s\n", e.ID, e.AmountULXC, e.Class,
				e.PayerAgentID, e.PayerWorkspaceID, e.PayeeAgentID, e.PayeeWorkspaceID, e.Memo, reason)
		}
		return nil
	case (args[0] == "release" || args[0] == "return") && len(args) >= 3:
		e, err := store.DecideEscrow(ctx, args[1], args[0] == "release", by, strings.Join(args[2:], " "))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s %s: %d µLXC\n", e.ID, e.Status, e.AmountULXC)
		return nil
	}
	return fmt.Errorf("%s", escrowsUsage)
}
