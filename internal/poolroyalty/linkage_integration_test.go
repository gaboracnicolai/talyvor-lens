package poolroyalty

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// linkageTestPool builds the mint schema + the U6 PR2 workspace_card_fingerprints
// table. Skips without LENS_TEST_DATABASE_URL.
func linkageTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG linkage test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP VIEW IF EXISTS pool_royalty_margin`,
		`DROP TABLE IF EXISTS pool_royalty_mints`,
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`DROP TABLE IF EXISTS workspace_card_fingerprints`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE pool_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE,
			requester_workspace_id TEXT NOT NULL, contributor_workspace_id TEXT NOT NULL, layer TEXT NOT NULL,
			entry_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			similarity DOUBLE PRECISION NOT NULL DEFAULT 0, avoided_cogs_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
			minted_amount BIGINT NOT NULL DEFAULT 0, answer_sha256 TEXT NOT NULL DEFAULT '',
			prompt_sha256 TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'final', finalize_after TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE workspace_card_fingerprints (workspace_id TEXT NOT NULL, fingerprint_hash TEXT NOT NULL,
			PRIMARY KEY (workspace_id, fingerprint_hash))`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func linkExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func washHit(reqID, contributor, requester string) ServedHit {
	return ServedHit{
		RequestID: reqID, RequesterWorkspace: requester, ContributorWorkspace: contributor,
		Layer: "exact", EntryID: "e1", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0, AnswerSHA256: SHA256Hex([]byte("a" + reqID)), PromptSHA256: SHA256Hex([]byte("p" + reqID)),
	}
}

func linkageMinter(pool *pgxpool.Pool) *Minter {
	m := NewMinter(pool, mining.NewLedgerStore(pool), 0.5, func() bool { return true })
	m.SetOwnerLinkageCheck(true)
	return m
}

// TestLinkage_SharedFingerprint_Denied — contributor A and requester B share a
// card fingerprint (same operator) → the pool-royalty mint is DENIED: no claim
// row, no held royalty. The wash is stopped at its root.
func TestLinkage_SharedFingerprint_Denied(t *testing.T) {
	pool := linkageTestPool(t)
	ctx := context.Background()
	m := linkageMinter(pool)
	linkExec(t, pool, `INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash)
		VALUES ('wsA','fp-same'), ('wsB','fp-same')`) // A and B paid with the same card

	res, err := m.MintServedHit(ctx, washHit("req-wash", "wsA", "wsB"))
	if err != nil {
		t.Fatalf("MintServedHit: %v", err)
	}
	if res.Minted {
		t.Fatal("a same-owner wash (shared fingerprint) must be DENIED, but it minted")
	}
	var claims int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pool_royalty_mints WHERE request_id='req-wash'`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 0 {
		t.Errorf("a denied wash must write NO claim row, got %d", claims)
	}
	var held int64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(held_balance,0) FROM lens_token_balances WHERE workspace_id='wsA'`).Scan(&held)
	if held != 0 {
		t.Errorf("a denied wash must mint NO held royalty, got %v", held)
	}
}

// TestLinkage_DifferentOrMissing_Allowed — different fingerprints, AND a missing
// fingerprint, are both INCONCLUSIVE → the mint is ALLOWED (default-allow on
// missing never blocks honest cross-actor reuse).
func TestLinkage_DifferentOrMissing_Allowed(t *testing.T) {
	pool := linkageTestPool(t)
	ctx := context.Background()
	m := linkageMinter(pool)
	linkExec(t, pool, `INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash)
		VALUES ('wsX','fp-x'), ('wsY','fp-y')`) // different cards

	if res, err := m.MintServedHit(ctx, washHit("req-diff", "wsX", "wsY")); err != nil || !res.Minted {
		t.Fatalf("different-fingerprint cross-actor reuse must be ALLOWED: res=%+v err=%v", res, err)
	}
	// requester wsZ has NO captured fingerprint → inconclusive → allowed.
	if res, err := m.MintServedHit(ctx, washHit("req-missing", "wsX", "wsZ_nofp")); err != nil || !res.Minted {
		t.Fatalf("inconclusive (missing fingerprint) must NOT block — default-allow: res=%+v err=%v", res, err)
	}
}

// TestLinkage_Disabled_AllowsSharedFingerprint — with the linkage check OFF (the
// default), even a shared fingerprint mints (proves the gate is the only thing
// denying, and existing tests are unaffected).
func TestLinkage_Disabled_AllowsSharedFingerprint(t *testing.T) {
	pool := linkageTestPool(t)
	ctx := context.Background()
	m := NewMinter(pool, mining.NewLedgerStore(pool), 0.5, func() bool { return true }) // linkage NOT enabled
	linkExec(t, pool, `INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash)
		VALUES ('wsA','fp-same'), ('wsB','fp-same')`)
	if res, err := m.MintServedHit(ctx, washHit("req-off", "wsA", "wsB")); err != nil || !res.Minted {
		t.Fatalf("linkage OFF must mint even a shared fingerprint: res=%+v err=%v", res, err)
	}
}
