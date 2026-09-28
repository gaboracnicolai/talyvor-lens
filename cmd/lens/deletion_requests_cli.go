package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/storedanswers"
)

const deletionRequestsUsage = `usage:
  lens deletion-requests                  list open requests
  lens deletion-requests all              list every request
  lens deletion-requests complete <id>    delete everything that workspace has stored, and mark it done
See docs/deletion-requests-runbook.md for the other stores to clear.`

// runDeletionRequests is `lens deletion-requests` (B21.3): the operator's side of "ask Talyvor to
// delete everything", run inside the lens container so it reaches the same Postgres and Redis.
func runDeletionRequests(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("deletion-requests: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("deletion-requests: database: %w", err)
	}
	defer pool.Close()
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("deletion-requests: redis: %w", err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()
	by := "operator-cli"
	if u := os.Getenv("USER"); u != "" {
		by += ":" + u
	}
	return deletionRequestsCommand(ctx, storedanswers.New(pool, rdb), args, by, os.Stdout)
}

func deletionRequestsCommand(ctx context.Context, store storedanswers.Deleter, args []string, by string, out io.Writer) error {
	switch {
	case len(args) == 0 || (len(args) == 1 && args[0] == "all"):
		reqs, err := store.List(ctx, len(args) == 0)
		if err != nil {
			return err
		}
		if len(reqs) == 0 {
			fmt.Fprintln(out, "no requests")
		}
		for _, r := range reqs {
			done := ""
			if r.CompletedAt != nil {
				done = " done " + r.CompletedAt.UTC().Format(time.RFC3339) + " by " + r.CompletedBy
			}
			fmt.Fprintf(out, "#%d  %s  workspace %s  requested %s by %s%s  %q\n", r.ID, r.Status, r.WorkspaceID,
				r.RequestedAt.UTC().Format(time.RFC3339), r.RequestedBy, done, r.Note)
		}
		return nil
	case len(args) == 2 && args[0] == "complete":
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("deletion-requests: %q is not a request id\n%s", args[1], deletionRequestsUsage)
		}
		r, c, err := store.Complete(ctx, id, by)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "#%d done %s — workspace %s: deleted %d shared and %d private answers, %d shared and %d private conversions, %d cached copies.\n%s\n",
			r.ID, r.CompletedAt.UTC().Format(time.RFC3339), r.WorkspaceID, c.SharedAnswers, c.PrivateAnswers,
			c.SharedConversions, c.PrivateConversions, c.CachedCopies, storedanswers.KeptRecords)
		fmt.Fprintln(out, "Now clear the other stores for this workspace: docs/deletion-requests-runbook.md.")
		return nil
	}
	return fmt.Errorf("deletion-requests: unknown arguments %q\n%s", args, deletionRequestsUsage)
}
