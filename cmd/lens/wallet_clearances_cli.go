package main

import (
	"context"
	"flag"
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
  lens clearances                                  every capability, its class, and its clearance
  lens clearances clear <capability> --licence <reference> --partner <name> --countries GB,IE --expires 2027-10-01 <reference>
                                                   let an AMBER or RED capability take real money: under that licence,
                                                   through that licensed partner, from those countries (ISO 3166-1
                                                   alpha-2), until that day, on the lawyer's or partner's reference
  lens clearances revoke <capability> <why>        stop it taking real money, from its next use
  lens clearances log                              every clear and revoke, newest first
(lens wallet-clearances is the same command.) See docs/wallet-capabilities.md.`

// walletClearanceAdmin is the slice of *economy.DualTokenStore the command needs.
type walletClearanceAdmin interface {
	WalletCapabilities(ctx context.Context) ([]economy.CapabilityStatus, error)
	ClearCapability(ctx context.Context, key, operator string, terms economy.ClearanceTerms) (economy.Clearance, error)
	RevokeClearance(ctx context.Context, key, operator, why string) error
	ClearanceLog(ctx context.Context, limit int) ([]economy.ClearanceRecord, error)
}

// runWalletClearances is `lens clearances` (B30.1; `lens wallet-clearances`, B22.1), run inside the lens container so it reaches the
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
				cl := c.Clearance
				state = fmt.Sprintf("real money from %s until %s: licence %s, partner %s, cleared %s by %s (%s)",
					strings.Join(cl.Countries, ","), cl.ExpiresAt.UTC().Format(time.RFC3339), cl.Licence, cl.Partner,
					cl.At.UTC().Format(time.RFC3339), cl.By, cl.Reference)
			case c.RealMoney:
				state = "real money"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", c.Class, c.Key, c.Name, state)
		}
		return nil
	case args[0] == "clear" && len(args) >= 2:
		terms, err := clearanceTerms(args[2:])
		if err != nil {
			return err
		}
		cl, err := store.ClearCapability(ctx, args[1], by, terms)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "cleared %s for real money from %s until %s (licence %s, partner %s) at %s by %s (%s)\n", args[1],
			strings.Join(cl.Countries, ","), cl.ExpiresAt.UTC().Format(time.RFC3339), cl.Licence, cl.Partner,
			cl.At.UTC().Format(time.RFC3339), cl.By, cl.Reference)
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

// clearanceTerms reads `--licence <reference> --partner <name> --countries GB,IE --expires 2027-10-01 <reference…>`.
// The expiry is a day (the clearance ends at its start, UTC) or an RFC 3339 time.
func clearanceTerms(args []string) (economy.ClearanceTerms, error) {
	fs := flag.NewFlagSet("clearances clear", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	licence := fs.String("licence", "", "")
	partner := fs.String("partner", "", "")
	countries := fs.String("countries", "", "")
	expires := fs.String("expires", "", "")
	if err := fs.Parse(args); err != nil {
		return economy.ClearanceTerms{}, fmt.Errorf("%w\n%s", err, walletClearancesUsage)
	}
	if *licence == "" || *partner == "" || *countries == "" || *expires == "" || fs.NArg() == 0 {
		return economy.ClearanceTerms{}, fmt.Errorf("a clearance names --licence, --partner, --countries, --expires and a reference\n%s", walletClearancesUsage)
	}
	at, err := time.Parse(time.DateOnly, *expires)
	if err != nil {
		if at, err = time.Parse(time.RFC3339, *expires); err != nil {
			return economy.ClearanceTerms{}, fmt.Errorf("--expires %q is not a day (2027-10-01) or an RFC 3339 time", *expires)
		}
	}
	return economy.ClearanceTerms{Reference: strings.Join(fs.Args(), " "), Licence: *licence, Partner: *partner,
		Countries: strings.Split(*countries, ","), ExpiresAt: at}, nil
}
