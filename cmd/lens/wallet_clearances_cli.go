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

const walletClearancesUsage = `usage:
  lens wallet-clearances                                  every capability, its class, and whether it takes real money
  lens wallet-clearances clear <capability> <reference>   let an AMBER or RED capability take real money, on the
                                                          lawyer's or partner's reference
  lens wallet-clearances revoke <capability> <why>        stop it taking real money, from its next use
  lens wallet-clearances log                              every clear and revoke, newest first
See docs/wallet-capabilities.md.`

// walletClearanceAdmin is the slice of *economy.DualTokenStore the command needs.
type walletClearanceAdmin interface {
	WalletCapabilities(ctx context.Context) ([]economy.CapabilityStatus, error)
	ClearCapability(ctx context.Context, key, operator, reference string) (economy.Clearance, error)
	RevokeClearance(ctx context.Context, key, operator, why string) error
	ClearanceLog(ctx context.Context, limit int) ([]economy.ClearanceRecord, error)
}

// runWalletClearances is `lens wallet-clearances` (B22.1), run inside the lens container so it reaches the
// same Postgres the server reads.
func runWalletClearances(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("wallet-clearances: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("wallet-clearances: database: %w", err)
	}
	defer pool.Close()
	by := "operator-cli"
	if u := os.Getenv("USER"); u != "" {
		by += ":" + u
	}
	return walletClearancesCommand(ctx, economy.NewDualTokenStore(nil, pool, nil), args, by, os.Stdout)
}

func walletClearancesCommand(ctx context.Context, store walletClearanceAdmin, args []string, by string, out io.Writer) error {
	switch {
	case len(args) == 0:
		caps, err := store.WalletCapabilities(ctx)
		if err != nil {
			return err
		}
		for _, c := range caps {
			state := "test money only"
			switch {
			case c.Clearance != nil:
				state = fmt.Sprintf("real money: cleared %s by %s (%s)", c.Clearance.At.UTC().Format(time.RFC3339), c.Clearance.By, c.Clearance.Reference)
			case c.RealMoney:
				state = "real money"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", c.Class, c.Key, c.Name, state)
		}
		return nil
	case args[0] == "clear" && len(args) >= 3:
		cl, err := store.ClearCapability(ctx, args[1], by, strings.Join(args[2:], " "))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "cleared %s for real money at %s by %s (%s)\n", args[1], cl.At.UTC().Format(time.RFC3339), cl.By, cl.Reference)
		return nil
	case args[0] == "revoke" && len(args) >= 3:
		if err := store.RevokeClearance(ctx, args[1], by, strings.Join(args[2:], " ")); err != nil {
			return err
		}
		fmt.Fprintf(out, "revoked: %s takes test money only from its next use\n", args[1])
		return nil
	case args[0] == "log" && len(args) == 1:
		log, err := store.ClearanceLog(ctx, 100)
		if err != nil {
			return err
		}
		if len(log) == 0 {
			fmt.Fprintln(out, "no clearances")
		}
		for _, r := range log {
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", r.At.UTC().Format(time.RFC3339), r.Action, r.Capability, r.Operator, r.Reference)
		}
		return nil
	}
	return fmt.Errorf("%s", walletClearancesUsage)
}
