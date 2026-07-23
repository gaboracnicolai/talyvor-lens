package poolroyalty

import (
	"context"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/migrations"
)

// latestViewDDL returns the CREATE VIEW statement for `view` from the
// HIGHEST-numbered migration that (re)defines it — the definition that actually
// ships. This binds the test to the migration CHAIN, not a local copy: a future
// migration that reverts the peg conversion turns this red automatically, and
// adding the peg-fix migration flips it green with no change to the test.
func latestViewDDL(t *testing.T, view string) string {
	t.Helper()
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // zero-padded NNNN_ prefix ⇒ lexical order == numeric order
	// (?is): case-insensitive, dot matches newline. Non-greedy .*?; stops at the
	// statement's terminating semicolon (the view body is a single SELECT).
	re := regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?VIEW\s+` + regexp.QuoteMeta(view) + `\s+AS.*?;`)
	ddl := ""
	for _, n := range names {
		raw, err := migrations.FS.ReadFile(n)
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		if m := re.FindAllString(string(raw), -1); len(m) > 0 {
			ddl = m[len(m)-1] // last (re)definition in the highest-numbered file wins
		}
	}
	if ddl == "" {
		t.Fatalf("no CREATE VIEW %s found in migrations/*.sql", view)
	}
	return ddl
}

// THE MARGIN, red-first: Talyvor's realized margin on a royalty is
// (1−s) × avoided_COGS in REAL DOLLARS. A $10 avoided at s=0.5 mints the
// contributor 50 LENS (= $5 of value at the $0.10 peg) and MUST leave $5 of
// margin — not 10 − 50 = −40, which is what the pre-peg view computed by
// treating the minted µLENS as if 1 LENS = $1.
//
// The view DDL comes from the SHIPPED migration (latestViewDDL), so this fails
// against the pre-fix chain and passes once the peg-fix migration lands.
func TestRoyaltyMarginViews_ShipPegDollars_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG margin view test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, ddl := range []string{
		`DROP VIEW IF EXISTS pool_royalty_margin`,
		`DROP VIEW IF EXISTS distill_royalty_margin`,
		`DROP TABLE IF EXISTS pool_royalty_mints`,
		`DROP TABLE IF EXISTS distill_royalty_mints`,
		`CREATE TABLE pool_royalty_mints (
			request_id TEXT PRIMARY KEY, requester_workspace_id TEXT NOT NULL,
			contributor_workspace_id TEXT NOT NULL, layer TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			avoided_cogs_usd DOUBLE PRECISION NOT NULL, minted_amount BIGINT NOT NULL,
			status TEXT NOT NULL DEFAULT 'final', created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE distill_royalty_mints (
			request_id TEXT PRIMARY KEY, requester_workspace_id TEXT NOT NULL,
			contributor_workspace_id TEXT NOT NULL, content_hash TEXT NOT NULL,
			avoided_cogs_usd DOUBLE PRECISION NOT NULL, minted_amount BIGINT NOT NULL,
			status TEXT NOT NULL DEFAULT 'final', created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		latestViewDDL(t, "pool_royalty_margin"),
		latestViewDDL(t, "distill_royalty_margin"),
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("setup DDL failed: %v\n%s", err, ddl)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP VIEW IF EXISTS pool_royalty_margin`)
		_, _ = pool.Exec(ctx, `DROP VIEW IF EXISTS distill_royalty_margin`)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS pool_royalty_mints`)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS distill_royalty_mints`)
	})

	// $10 avoided, 50 LENS minted (= $5 of value at the peg), realized (FINAL).
	if _, err := pool.Exec(ctx,
		`INSERT INTO pool_royalty_mints (request_id, requester_workspace_id, contributor_workspace_id, layer, avoided_cogs_usd, minted_amount, status)
		 VALUES ('p1','wsB','wsA','exact',10.0,$1,'final')`, micro(50)); err != nil {
		t.Fatalf("insert pool row: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO distill_royalty_mints (request_id, requester_workspace_id, contributor_workspace_id, content_hash, avoided_cogs_usd, minted_amount, status)
		 VALUES ('d1','wsB','wsA','h1',10.0,$1,'final')`, micro(50)); err != nil {
		t.Fatalf("insert distill row: %v", err)
	}

	for _, tc := range []struct{ view, id string }{
		{"pool_royalty_margin", "p1"},
		{"distill_royalty_margin", "d1"},
	} {
		var margin float64
		if err := pool.QueryRow(ctx,
			`SELECT margin_usd FROM `+tc.view+` WHERE request_id=$1`, tc.id).Scan(&margin); err != nil {
			t.Fatalf("read %s.margin_usd: %v", tc.view, err)
		}
		if math.Abs(margin-5.0) > 1e-9 {
			t.Fatalf("%s.margin_usd = $%v for a $10 avoided / 50 LENS minted row, want $5 ((1−s)×avoided at the $0.10 peg)", tc.view, margin)
		}
	}
}
