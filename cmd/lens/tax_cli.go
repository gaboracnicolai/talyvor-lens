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
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/platformreport"
)

const taxUsage = `usage:
  lens tax rates                                   every tax rate loaded
  lens tax rates import <file.csv>                 load tax rates and their jurisdictions; a rate already loaded for
                                                   the same jurisdiction, tax code and start is replaced. The file's
                                                   header is:
                                                   ` + partners.TaxRatesCSVHeader + `
                                                   rate_bps is basis points (100 is 1%), dates are YYYY-MM-DD (UTC),
                                                   valid_to and union may be empty
  lens tax registrations                           every registration Talyvor holds
  lens tax registrations add <jurisdiction> --scheme <scheme> --number <number> --from <YYYY-MM-DD>
                                                   record one of Talyvor's registrations, such as
                                                   GB --scheme "GB VAT", or EU --scheme "EU OSS non-Union"
` + taxReturnUsage

// taxAdmin is the slice of *partners.TaxStore the command needs.
type taxAdmin interface {
	ImportRates(ctx context.Context, rows []partners.TaxRateRow) (rates, jurisdictions int, err error)
	AddRegistration(ctx context.Context, r partners.TaxRegistration) error
	Rates(ctx context.Context) ([]partners.TaxRate, error)
	Registrations(ctx context.Context) ([]partners.TaxRegistration, error)
}

// runTax is `lens tax` (B32.37; `lens tax return`, B32.44), run inside the lens container so it reaches the same
// Postgres the server reads.
func runTax(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("tax: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("tax: database: %w", err)
	}
	defer pool.Close()
	if len(args) > 0 && args[0] == "return" {
		return taxReturnCommand(ctx, platformreport.New(pool, nil), args[1:], os.Stdout)
	}
	return taxCommand(ctx, partners.NewTaxStore(pool), args, os.Stdout)
}

func taxCommand(ctx context.Context, store taxAdmin, args []string, out io.Writer) error {
	switch {
	case len(args) == 1 && args[0] == "rates":
		rates, err := store.Rates(ctx)
		if err != nil {
			return err
		}
		if len(rates) == 0 {
			fmt.Fprintln(out, "no tax rates loaded")
		}
		for _, r := range rates {
			to := "open"
			if r.ValidTo != nil {
				to = r.ValidTo.UTC().Format(time.DateOnly)
			}
			fmt.Fprintf(out, "%s\t%s\t%d bps\t%s to %s\t%s\n", r.Jurisdiction, r.TaxCode, r.RateBps,
				r.ValidFrom.UTC().Format(time.DateOnly), to, r.Source)
		}
		return nil
	case len(args) == 3 && args[0] == "rates" && args[1] == "import":
		f, err := os.Open(args[2])
		if err != nil {
			return err
		}
		defer f.Close()
		rows, err := partners.ParseTaxRatesCSV(f)
		if err != nil {
			return err
		}
		rates, jurisdictions, err := store.ImportRates(ctx, rows)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "imported %d tax rates in %d jurisdictions\n", rates, jurisdictions)
		return nil
	case len(args) == 1 && args[0] == "registrations":
		regs, err := store.Registrations(ctx)
		if err != nil {
			return err
		}
		if len(regs) == 0 {
			fmt.Fprintln(out, "no tax registrations")
		}
		for _, r := range regs {
			fmt.Fprintf(out, "%s\t%s\t%s\tfrom %s\n", r.Jurisdiction, r.Scheme, r.Number, r.EffectiveFrom.UTC().Format(time.DateOnly))
		}
		return nil
	case len(args) >= 3 && args[0] == "registrations" && args[1] == "add":
		r, err := taxRegistration(args[2:])
		if err != nil {
			return err
		}
		if err := store.AddRegistration(ctx, r); err != nil {
			return err
		}
		fmt.Fprintf(out, "registered in %s under %s, number %s, from %s\n", r.Jurisdiction, r.Scheme, r.Number,
			r.EffectiveFrom.Format(time.DateOnly))
		return nil
	}
	return fmt.Errorf("%s", taxUsage)
}

// taxRegistration reads `<jurisdiction> --scheme <scheme> --number <number> --from <YYYY-MM-DD>`.
func taxRegistration(args []string) (partners.TaxRegistration, error) {
	fs := flag.NewFlagSet("tax registrations add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scheme := fs.String("scheme", "", "")
	number := fs.String("number", "", "")
	from := fs.String("from", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return partners.TaxRegistration{}, fmt.Errorf("%w\n%s", err, taxUsage)
	}
	if *scheme == "" || *number == "" || *from == "" || fs.NArg() != 0 {
		return partners.TaxRegistration{}, fmt.Errorf("a registration names its jurisdiction, --scheme, --number and --from\n%s", taxUsage)
	}
	at, err := time.Parse(time.DateOnly, *from)
	if err != nil {
		return partners.TaxRegistration{}, fmt.Errorf("--from %q is not a day (2026-01-01)", *from)
	}
	return partners.TaxRegistration{Jurisdiction: strings.ToUpper(args[0]), Scheme: *scheme, Number: *number, EffectiveFrom: at}, nil
}
