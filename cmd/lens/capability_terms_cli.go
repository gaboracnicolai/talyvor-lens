package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	capabilityterms "github.com/talyvor/lens/docs/terms"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/economy"
)

const capabilityTermsUsage = `usage:
  lens terms                          every capability with terms, its latest version and when it was published
  lens terms publish <capability>     publish this build's docs/terms/<capability>.md as the capability's next version:
                                      every workspace must accept it before its next use
See internal/economy/capability_terms.go.`

// capabilityTermsAdmin is the slice of *economy.DualTokenStore the command needs.
type capabilityTermsAdmin interface {
	WorkspaceTermsList(ctx context.Context, workspaceID string) ([]economy.WorkspaceTerms, error)
	PublishTerms(ctx context.Context, capability, by string, text economy.TermsText) (economy.CapabilityTerms, error)
}

// runCapabilityTerms is `lens terms` (B30.9), run inside the lens container so it reaches the same Postgres the server
// reads and publishes the texts the running build carries.
func runCapabilityTerms(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("terms: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("terms: database: %w", err)
	}
	defer pool.Close()
	by := "operator-cli"
	if u := os.Getenv("USER"); u != "" {
		by += ":" + u
	}
	return capabilityTermsCommand(ctx, economy.NewDualTokenStore(nil, pool, nil), economy.CapabilityTermsTexts(capabilityterms.FS),
		args, by, os.Stdout)
}

func capabilityTermsCommand(ctx context.Context, store capabilityTermsAdmin, texts map[string]economy.TermsText, args []string,
	by string, out io.Writer) error {
	switch {
	case len(args) == 0:
		ts, err := store.WorkspaceTermsList(ctx, "")
		if err != nil {
			return err
		}
		if len(ts) == 0 {
			fmt.Fprintln(out, "no capability has terms")
		}
		for _, t := range ts {
			fmt.Fprintf(out, "%s\tversion %d\t%s\tpublished %s by %s\n", t.Capability, t.Version, t.TextPath,
				t.PublishedAt.UTC().Format(time.RFC3339), t.PublishedBy)
		}
		return nil
	case args[0] == "publish" && len(args) == 2:
		text, ok := texts[args[1]]
		if !ok {
			return fmt.Errorf("this build carries no docs/terms/%s.md", args[1])
		}
		t, err := store.PublishTerms(ctx, args[1], by, text)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "published version %d of %s's terms (%s) at %s by %s: every workspace accepts it before its next use\n",
			t.Version, t.Capability, t.TextPath, t.PublishedAt.UTC().Format(time.RFC3339), t.PublishedBy)
		return nil
	}
	return fmt.Errorf("%s", capabilityTermsUsage)
}
