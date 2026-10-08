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
	"github.com/talyvor/lens/internal/economy"
)

const verificationLimitsUsage = `usage:
  lens verification-limits                         each level, what it means, the capabilities that need it, and its
                                                   live limit in each currency (none: no live money at that level)
  lens verification-limits set <L1|L2|L3> <GBP|EUR|USD|USDC> <amount> <reference>
                                                   the most one movement of live money may move at that level, in
                                                   that currency, e.g. 1000.00; 0 takes none
A level with no limit of its own in a currency has the highest one set below it. See docs/wallet-capabilities.md.`

// verificationLimitsAdmin is the slice of *economy.DualTokenStore the command needs.
type verificationLimitsAdmin interface {
	LevelLimits(ctx context.Context) ([]economy.LevelLimit, error)
	SetLevelLimit(ctx context.Context, level economy.VerificationLevel, currency string, limitMinor int64, operator, reference string) (economy.LevelLimit, error)
}

// runVerificationLimits is `lens verification-limits` (B30.4), run inside the lens container so it reaches the same
// Postgres the server reads.
func runVerificationLimits(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("verification-limits: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("verification-limits: database: %w", err)
	}
	defer pool.Close()
	by := "operator-cli"
	if u := os.Getenv("USER"); u != "" {
		by += ":" + u
	}
	return verificationLimitsCommand(ctx, economy.NewDualTokenStore(nil, pool, nil), args, by, os.Stdout)
}

func verificationLimitsCommand(ctx context.Context, store verificationLimitsAdmin, args []string, by string, out io.Writer) error {
	switch {
	case len(args) == 0:
		limits, err := store.LevelLimits(ctx)
		if err != nil {
			return err
		}
		for level := economy.LevelSignedIn; level <= economy.LevelCompany; level++ {
			var needs []string
			for _, c := range economy.Capabilities {
				if c.Level == level {
					needs = append(needs, c.Key)
				}
			}
			fmt.Fprintf(out, "%s\t%s\tneeded by: %s\n", level, level.Meaning(), strings.Join(needs, ", "))
			if level == economy.LevelSignedIn {
				continue
			}
			for _, cur := range []string{economy.CurrencyGBP, economy.CurrencyEUR, economy.CurrencyUSD, economy.CurrencyUSDC} {
				state := "none — no live money"
				for _, l := range limits {
					if l.Level == level && l.Currency == cur {
						state = fmt.Sprintf("%s per movement, set %s by %s (%s)", economy.FormatMinor(l.LimitMinor, cur),
							l.At.UTC().Format(time.RFC3339), l.Operator, l.Reference)
					}
				}
				fmt.Fprintf(out, "\t%s\t%s\n", cur, state)
			}
		}
		return nil
	case args[0] == "set" && len(args) >= 5:
		level, err := economy.ParseVerificationLevel(args[1])
		if err != nil {
			return err
		}
		cur := strings.ToUpper(args[2])
		minor, err := decimalToMinor(args[3], cur)
		if err != nil {
			return err
		}
		l, err := store.SetLevelLimit(ctx, level, cur, minor, by, strings.Join(args[4:], " "))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s may move %s of live money in one movement, from now (set by %s: %s)\n", l.Level,
			economy.FormatMinor(l.LimitMinor, l.Currency), l.Operator, l.Reference)
		return nil
	}
	return fmt.Errorf("%s", verificationLimitsUsage)
}

// decimalToMinor reads an amount in a currency's major units — "1000", "1000.5", "1000.50" — as its minor units,
// exactly: never through a float.
func decimalToMinor(s, currency string) (int64, error) {
	places, ok := economy.MoneyCurrencies[currency]
	if !ok {
		return 0, fmt.Errorf("a limit is in GBP, EUR, USD or USDC, not %q", currency)
	}
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if len(frac) > places {
		return 0, fmt.Errorf("%q has more decimal places than %s has (%d)", s, currency, places)
	}
	digits := whole + frac + strings.Repeat("0", places-len(frac))
	if whole == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not an amount like 1000.00", s)
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is too large an amount", s)
	}
	return n, nil
}
