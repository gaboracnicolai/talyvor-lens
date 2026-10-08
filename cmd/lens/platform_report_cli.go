package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/platformreport"
	"github.com/talyvor/lens/internal/sellertax"
)

const platformReportUsage = `usage:
  lens platform-report --year <YYYY> [--test] [--out <dir>] [--operator <name>]
      write the year's platform-reporting export — one record per seller resident in the UK or an EU member state —
      as platform-report-<YYYY>.csv and platform-report-<YYYY>.json in --out (default the current directory), and
      record each file's sha256 in platform_reports and the operator audit trail. --test reports test money, into
      platform-report-<YYYY>-test.*; --operator names who ran it (default $USER). The files hold the sellers' TINs,
      dates of birth and account numbers in clear: hand them to Talyvor's accountant and nobody else.
      docs/platform-reporting.md documents the file.`

// runPlatformReport is `lens platform-report` (B32.44), run inside the lens container so it reaches the same Postgres
// and the same LENS_PROVIDER_SECRET_KEK the server seals the sellers' details with.
func runPlatformReport(args []string) error {
	fs := flag.NewFlagSet("platform-report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	year := fs.Int("year", 0, "")
	test := fs.Bool("test", false, "")
	out := fs.String("out", ".", "")
	operator := fs.String("operator", os.Getenv("USER"), "")
	if err := fs.Parse(args); err != nil || *year == 0 || fs.NArg() != 0 {
		return fmt.Errorf("%s", platformReportUsage)
	}
	if strings.TrimSpace(*operator) == "" {
		return fmt.Errorf("--operator names who is running the export\n%s", platformReportUsage)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("platform-report: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("platform-report: database: %w", err)
	}
	defer pool.Close()
	g := platformreport.New(pool, sellertax.NewStore(pool, cfg.ProviderSecretKeyring, partners.NewRegistry(nil)))
	return platformReportCommand(ctx, g, *year, *test, *out, *operator, os.Stdout)
}

func platformReportCommand(ctx context.Context, g *platformreport.Generator, year int, test bool, dir, operator string, out io.Writer) error {
	funding, name := platformreport.FundingLive, fmt.Sprintf("platform-report-%d", year)
	if test {
		funding, name = platformreport.FundingTest, name+"-test"
	}
	rep, err := g.Generate(ctx, year, funding)
	if err != nil {
		return err
	}
	for _, format := range []string{platformreport.FormatCSV, platformreport.FormatJSON} {
		file, err := platformreport.Render(rep, format)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, name+"."+format)
		if err := os.WriteFile(path, file, 0o600); err != nil {
			return fmt.Errorf("platform-report: write %s: %w", path, err)
		}
		run, err := g.Record(ctx, rep, format, file, operator)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\t%d records\tsha256 %s\t%s\n", path, run.Rows, run.SHA256, run.ID)
	}
	if len(rep.Unresolved) > 0 {
		fmt.Fprintf(out, "not yet reportable — credited in %d with no country of residence on file: %s\n",
			year, strings.Join(rep.Unresolved, ", "))
	}
	return nil
}

const taxReturnUsage = `  lens tax return --jurisdiction <GB|EU|member state> --quarter <YYYYQn> [--test] [--json]
                                                   the quarter's figures for the UK VAT return (GB) or the EU OSS
                                                   return (EU, or one member state): the tax lines of the sales
                                                   cleared in it, less those refunded in it, by jurisdiction,
                                                   treatment and rate, in µUSD`

// taxReturnCommand is `lens tax return` (B32.44).
func taxReturnCommand(ctx context.Context, g *platformreport.Generator, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tax return", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jurisdiction := fs.String("jurisdiction", "", "")
	quarter := fs.String("quarter", "", "")
	test := fs.Bool("test", false, "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || *jurisdiction == "" || *quarter == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage:\n%s", taxReturnUsage)
	}
	funding := platformreport.FundingLive
	if *test {
		funding = platformreport.FundingTest
	}
	ret, err := g.TaxReturn(ctx, *jurisdiction, *quarter, funding)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(ret)
	}
	fmt.Fprintf(out, "%s %s (%s money), %s to %s, µUSD\n", ret.Jurisdiction, ret.Quarter, ret.Funding,
		ret.From.Format(time.DateOnly), ret.To.AddDate(0, 0, -1).Format(time.DateOnly))
	fmt.Fprintln(out, "jurisdiction\ttreatment\trate_bps\tsales\ttaxable\ttax\trefunds\trefunded_taxable\trefunded_tax\tnet_taxable\tnet_tax")
	for _, l := range ret.Lines {
		fmt.Fprintf(out, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", l.Jurisdiction, l.Treatment, l.RateBps, l.Sales,
			l.TaxableUSDMicros, l.TaxUSDMicros, l.Refunds, l.RefundedTaxableUSDMicros, l.RefundedTaxUSDMicros,
			l.NetTaxableUSDMicros, l.NetTaxUSDMicros)
	}
	fmt.Fprintf(out, "total\t\t\t\t\t\t\t\t\t%d\t%d\n", ret.TaxableUSDMicros, ret.TaxUSDMicros)
	fmt.Fprintf(out, "the journal's tax accounts: %d\n", ret.JournalTaxUSDMicros)
	if ret.JournalTaxUSDMicros != ret.TaxUSDMicros {
		fmt.Fprintln(out, "⚠ the tax lines and the journal differ: a use cleared before its tax was billed")
	}
	return nil
}
