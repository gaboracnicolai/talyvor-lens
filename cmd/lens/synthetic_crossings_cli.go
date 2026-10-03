package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/economy"
)

const syntheticCrossingsUsage = `usage:
  lens synthetic-crossings             every transfer, escrow and market use between a test workspace and a real one,
                                       oldest first, and whether it is left to reverse. It changes nothing.
  lens synthetic-crossings --reverse   reverse each one left, once: the agent that was paid gives it back, with a
                                       ledger entry on both sides naming the original row`

// crossingAdmin is the slice of *economy.DualTokenStore the command needs.
type crossingAdmin interface {
	TestCrossings(ctx context.Context) ([]economy.TestCrossing, error)
	ReverseTestCrossing(ctx context.Context, c economy.TestCrossing, operator string) error
}

// runSyntheticCrossings is `lens synthetic-crossings` (B26.15), run inside the lens container so it reaches the
// same Postgres the server reads.
func runSyntheticCrossings(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("synthetic-crossings: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("synthetic-crossings: database: %w", err)
	}
	defer pool.Close()
	by := "operator-cli"
	if u := os.Getenv("USER"); u != "" {
		by += ":" + u
	}
	return syntheticCrossingsCommand(ctx, economy.NewDualTokenStore(nil, pool, nil), args, by, os.Stdout)
}

func syntheticCrossingsCommand(ctx context.Context, store crossingAdmin, args []string, by string, out io.Writer) error {
	reverse := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "--reverse":
		reverse = true
	default:
		return fmt.Errorf("%s", syntheticCrossingsUsage)
	}
	list, err := store.TestCrossings(ctx)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(out, "no money crossed between a test workspace and a real one")
		return nil
	}
	left, failed := 0, 0
	for _, c := range list {
		status := "TO REVERSE"
		switch {
		case c.ReversedAt != nil:
			status = "reversed " + c.ReversedAt.UTC().Format(time.RFC3339)
		case c.Settled != "":
			status = "nothing to reverse: " + c.Settled
		default:
			left++
		}
		fmt.Fprintf(out, "%s\t%s\t%d µLXC\tfrom %s\tto %s\t%s\t%s\t%s\n", c.Source, c.ID, c.AmountULXC,
			crossingSide(c.FromWorkspaceID, c.FromAgentID, c.FromTest), crossingSide(c.ToWorkspaceID, c.ToAgentID, !c.FromTest),
			c.State, c.CreatedAt.UTC().Format(time.RFC3339), status)
		if !reverse || c.ReversedAt != nil || c.Settled != "" {
			continue
		}
		if err := store.ReverseTestCrossing(ctx, c, by); err != nil {
			failed++
			fmt.Fprintf(out, "  NOT REVERSED: %v\n", err)
			continue
		}
		fmt.Fprintf(out, "  REVERSED: %d µLXC taken back from %s and returned to %s\n", c.AmountULXC, c.ToWorkspaceID, c.FromWorkspaceID)
		if c.Metered && !c.FromTest {
			fmt.Fprintf(out, "  ⚠ it was metered on the real buyer %s's marketplace bill: credit it there in Stripe\n", c.FromWorkspaceID)
		}
	}
	switch {
	case !reverse:
		fmt.Fprintf(out, "%d crossings, %d left to reverse (lens synthetic-crossings --reverse)\n", len(list), left)
	case failed > 0:
		return fmt.Errorf("synthetic-crossings: %d of %d left could not be reversed (above)", failed, left)
	default:
		fmt.Fprintf(out, "%d crossings, %d reversed now\n", len(list), left)
	}
	return nil
}

func crossingSide(ws, agent string, test bool) string {
	kind := "real"
	if test {
		kind = "test"
	}
	if agent == "" {
		return fmt.Sprintf("%s (%s)", ws, kind)
	}
	return fmt.Sprintf("%s (%s) agent %s", ws, kind, agent)
}
