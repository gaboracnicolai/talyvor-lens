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
	"github.com/talyvor/lens/internal/market"
)

const marketUsage = `usage:
  lens market journal-check               reconcile every seller's marketplace journal with their earnings and payouts
  lens market journal-check <workspace>   one seller's
A seller that does not reconcile is printed with why, and the command exits 1.`

// journalChecker is the slice of *market.Store the command needs.
type journalChecker interface {
	JournalSellers(ctx context.Context) ([]string, error)
	JournalCheck(ctx context.Context, workspaceID string) (market.JournalReconciliation, error)
}

// runMarket is `lens market` (B32.17), run inside the lens container so it reaches the same Postgres the server
// reads.
func runMarket(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("market: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("market: database: %w", err)
	}
	defer pool.Close()
	return marketCommand(ctx, market.NewStore(pool), args, os.Stdout)
}

func marketCommand(ctx context.Context, store journalChecker, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "journal-check" || len(args) > 2 {
		return fmt.Errorf("%s", marketUsage)
	}
	sellers := args[1:]
	if len(sellers) == 0 {
		var err error
		if sellers, err = store.JournalSellers(ctx); err != nil {
			return err
		}
	}
	bad := 0
	for _, ws := range sellers {
		r, err := store.JournalCheck(ctx, ws)
		if err != nil {
			return err
		}
		if !r.OK() {
			bad++
			fmt.Fprintf(out, "%s\tDOES NOT RECONCILE\t%s\n", ws, strings.Join(r.Mismatches, "; "))
			continue
		}
		fmt.Fprintf(out, "%s\tok\tholdback %s\tavailable %s\tdue for release %s\n", ws,
			usdMicros(r.InHoldbackUSDMicros), usdMicros(r.AvailableUSDMicros), usdMicros(r.PendingReleaseUSDMicros))
	}
	fmt.Fprintf(out, "%d sellers checked, %d do not reconcile\n", len(sellers), bad)
	if bad > 0 {
		return fmt.Errorf("market journal-check: %d of %d sellers do not reconcile", bad, len(sellers))
	}
	return nil
}

// usdMicros writes µUSD as dollars to the micro-dollar: 1234567 is $1.234567, −850000 is −$0.85.
func usdMicros(m int64) string {
	sign := ""
	if m < 0 {
		sign, m = "-", -m
	}
	frac := strings.TrimRight(fmt.Sprintf("%06d", m%1_000_000), "0")
	if len(frac) < 2 {
		frac += strings.Repeat("0", 2-len(frac))
	}
	return fmt.Sprintf("%s$%d.%s", sign, m/1_000_000, frac)
}
