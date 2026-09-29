package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/economy"
)

const creditLinesUsage = `usage:
  lens credit-lines                               every company's credit line: limit, used, available, paused
  lens credit-lines company <workspace> on|off    record whether a workspace is a company
  lens credit-lines set <workspace> <limit LXC>   give a company a credit line, or change its limit
  lens credit-lines pause <workspace> <why>       stop the line lending
  lens credit-lines resume <workspace>            let it lend again (an unpaid late invoice still pauses it)
See docs/credit-lines.md.`

// creditLineAdmin is the slice of *economy.DualTokenStore the command needs.
type creditLineAdmin interface {
	ListCreditLines(ctx context.Context) ([]economy.CreditLine, error)
	SetCompany(ctx context.Context, workspaceID string, company bool) error
	SetCreditLine(ctx context.Context, workspaceID string, limit int64, by string) (economy.CreditLine, error)
	PauseCreditLine(ctx context.Context, workspaceID, why string) error
	ResumeCreditLine(ctx context.Context, workspaceID string) error
}

// runCreditLines is `lens credit-lines` (B22.4), run inside the lens container so it reaches the same Postgres
// the server reads.
func runCreditLines(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("credit-lines: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("credit-lines: database: %w", err)
	}
	defer pool.Close()
	by := "operator-cli"
	if u := os.Getenv("USER"); u != "" {
		by += ":" + u
	}
	return creditLinesCommand(ctx, economy.NewDualTokenStore(nil, pool, nil), args, by, os.Stdout)
}

func creditLinesCommand(ctx context.Context, store creditLineAdmin, args []string, by string, out io.Writer) error {
	lxc := func(ulxc int64) string { return strconv.FormatFloat(float64(ulxc)/1e6, 'f', -1, 64) + " LXC" }
	switch {
	case len(args) == 0:
		lines, err := store.ListCreditLines(ctx)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			fmt.Fprintln(out, "no credit lines")
		}
		for _, l := range lines {
			state := "lending"
			if l.Paused {
				state = "paused: " + l.PausedReason
			}
			fmt.Fprintf(out, "%s\tlimit %s\tused %s\tavailable %s\t%s\n", l.WorkspaceID, lxc(l.LimitULXC), lxc(l.UsedULXC), lxc(l.AvailableULXC), state)
		}
		return nil
	case args[0] == "company" && len(args) == 3 && (args[2] == "on" || args[2] == "off"):
		if err := store.SetCompany(ctx, args[1], args[2] == "on"); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s company: %s\n", args[1], args[2])
		return nil
	case args[0] == "set" && len(args) == 3:
		limit, err := strconv.ParseFloat(args[2], 64)
		if err != nil || limit <= 0 {
			return fmt.Errorf("credit-lines: the limit must be a positive number of LXC, not %q", args[2])
		}
		l, err := store.SetCreditLine(ctx, args[1], int64(math.Round(limit*1e6)), by)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: credit line of %s (used %s)\n", l.WorkspaceID, lxc(l.LimitULXC), lxc(l.UsedULXC))
		return nil
	case args[0] == "pause" && len(args) >= 3:
		if err := store.PauseCreditLine(ctx, args[1], strings.Join(args[2:], " ")); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: credit line paused\n", args[1])
		return nil
	case args[0] == "resume" && len(args) == 2:
		if err := store.ResumeCreditLine(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: credit line resumed\n", args[1])
		return nil
	}
	return fmt.Errorf("%s", creditLinesUsage)
}
