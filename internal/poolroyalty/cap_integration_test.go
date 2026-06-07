package poolroyalty

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// THE CRITICAL CAP CORRECTNESS TEST — exactness under real concurrency.
//
// pgxmock cannot exhibit the property the cap design rests on (FOR UPDATE
// lock-wait serialization + READ COMMITTED per-statement snapshots), so this
// test runs against a REAL Postgres, gated exactly like the dbmigrate
// integration tests: set LENS_TEST_DATABASE_URL to a THROWAWAY database to
// enable it; the default unit suite skips it and stays hermetic.
//
// It fires N concurrent MintServedHit calls for the SAME (requester,
// contributor) pair with DISTINCT request_ids against a cap of K and asserts
// EXACTLY K mint, the rest Capped — proving the after-CreditTx count is
// race-free: every mint for a pair serializes on the contributor-balance
// FOR UPDATE inside CreditTx, and the count (a fresh READ COMMITTED
// snapshot taken after that lock was acquired) therefore always sees every
// prior committed mint for the pair.
func TestCapExactness_ConcurrentSamePair_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG cap exactness test")
	}
	ctx := context.Background()
	// Force a wide pool so the 25 goroutines genuinely contend (the default
	// pool sizes to ~CPU count, which would soften the race probe).
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	cfg.MaxConns = 25
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	// Minimal real schema for the mint tx — the three tables MintServedHit's
	// SQL touches, shaped as migrations 0019/0043/0045 define them (vanilla
	// PG; no pgvector needed). Throwaway DB: drop + recreate for determinism.
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS pool_royalty_mints`,
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`CREATE TABLE lens_token_balances (
			workspace_id    TEXT PRIMARY KEY,
			balance         DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_earned DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_spent  DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE lens_token_ledger (
			id            UUID             NOT NULL DEFAULT gen_random_uuid(),
			workspace_id  TEXT             NOT NULL,
			amount        DOUBLE PRECISION NOT NULL,
			balance_after DOUBLE PRECISION NOT NULL,
			type          TEXT             NOT NULL,
			description   TEXT             NOT NULL DEFAULT '',
			metadata      JSONB            NOT NULL DEFAULT '{}'::jsonb,
			created_at    TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
			PRIMARY KEY (id, workspace_id)
		)`,
		`CREATE TABLE pool_royalty_mints (
			id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			request_id               TEXT NOT NULL UNIQUE,
			requester_workspace_id   TEXT NOT NULL,
			contributor_workspace_id TEXT NOT NULL,
			layer                    TEXT NOT NULL,
			entry_id                 TEXT NOT NULL DEFAULT '',
			provider                 TEXT NOT NULL DEFAULT '',
			model                    TEXT NOT NULL DEFAULT '',
			similarity               DOUBLE PRECISION NOT NULL DEFAULT 0,
			avoided_cogs_usd         DOUBLE PRECISION NOT NULL DEFAULT 0,
			minted_amount            DOUBLE PRECISION NOT NULL DEFAULT 0,
			answer_sha256            TEXT NOT NULL DEFAULT '',
			prompt_sha256            TEXT NOT NULL DEFAULT '',
			created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}

	const (
		capK       = 5
		concurrent = 25
	)
	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetCap(capK, time.Hour)

	var wg sync.WaitGroup
	results := make([]Result, concurrent)
	errs := make([]error, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := ServedHit{
				RequestID:            fmt.Sprintf("req-cap-race-%02d", i),
				RequesterWorkspace:   "wsB",
				ContributorWorkspace: "wsA",
				Layer:                "exact",
				EntryID:              "lens:exact:cap-race-entry",
				Provider:             "openai",
				Model:                "gpt-4o",
				AvoidedCOGSUSD:       2.0,
				AnswerSHA256:         SHA256Hex([]byte("answer")),
				PromptSHA256:         SHA256Hex([]byte("prompt")),
			}
			results[i], errs[i] = m.MintServedHit(ctx, h)
		}(i)
	}
	wg.Wait()

	var minted, capped int
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("serve %d errored: %v", i, errs[i])
		}
		switch {
		case results[i].Minted:
			minted++
		case results[i].Capped:
			capped++
		default:
			t.Errorf("serve %d neither minted nor capped: %+v", i, results[i])
		}
	}
	if minted != capK || capped != concurrent-capK {
		t.Fatalf("EXACTNESS VIOLATED: minted=%d capped=%d, want exactly %d/%d — the after-CreditTx count raced", minted, capped, capK, concurrent-capK)
	}

	// DB ground truth must agree with the in-process tally.
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pool_royalty_mints`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != capK {
		t.Fatalf("claim rows=%d, want exactly %d (capped attempts must leave NO rows)", rows, capK)
	}
	var bal float64
	if err := pool.QueryRow(ctx, `SELECT balance FROM lens_token_balances WHERE workspace_id='wsA'`).Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if want := float64(capK) * 0.5 * 2.0; bal != want {
		t.Fatalf("contributor balance=%v, want %v (exactly K credits — exposure = cap × s × avoided_COGS)", bal, want)
	}
}
