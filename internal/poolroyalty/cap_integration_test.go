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
		`DROP VIEW IF EXISTS pool_royalty_margin`,
		`DROP TABLE IF EXISTS pool_royalty_mints`,
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`CREATE TABLE lens_token_balances (
			workspace_id    TEXT PRIMARY KEY,
			balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0,
			lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE lens_token_ledger (
			id            UUID             NOT NULL DEFAULT gen_random_uuid(),
			workspace_id  TEXT             NOT NULL,
			amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL,
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
			minted_amount BIGINT NOT NULL DEFAULT 0,
			answer_sha256            TEXT NOT NULL DEFAULT '',
			prompt_sha256            TEXT NOT NULL DEFAULT '',
			status                   TEXT NOT NULL DEFAULT 'final',
			finalize_after           TIMESTAMPTZ,
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
	var bal, held int64
	if err := pool.QueryRow(ctx, `SELECT balance, held_balance FROM lens_token_balances WHERE workspace_id='wsA'`).Scan(&bal, &held); err != nil {
		t.Fatal(err)
	}
	if want := int64(capK) * micro(1); held != want || bal != 0 {
		t.Fatalf("contributor held=%v balance=%v, want held=%v balance=0 (2.3a: exactly K HELD credits, nothing spendable until finalize)", held, bal, want)
	}
}

// STAGE 2.3a INTEGRATION — the holdback lifecycle on real Postgres:
// held-credit at mint, supply-at-finalize, concurrent double-finalize
// impossibility, revoke-not-burned, and the sweeper-not-flag-gated property.
func TestHoldbackLifecycle_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG holdback test")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	for _, ddl := range []string{
		`DROP VIEW IF EXISTS pool_royalty_margin`,
		`DROP TABLE IF EXISTS pool_royalty_mints`,
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`CREATE TABLE lens_token_balances (
			workspace_id    TEXT PRIMARY KEY,
			balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0,
			lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0,
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE lens_token_ledger (
			id            UUID             NOT NULL DEFAULT gen_random_uuid(),
			workspace_id  TEXT             NOT NULL,
			amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL,
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
			minted_amount BIGINT NOT NULL DEFAULT 0,
			answer_sha256            TEXT NOT NULL DEFAULT '',
			prompt_sha256            TEXT NOT NULL DEFAULT '',
			status                   TEXT NOT NULL DEFAULT 'final',
			finalize_after           TIMESTAMPTZ,
			created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE OR REPLACE VIEW pool_royalty_margin AS
		SELECT request_id, requester_workspace_id, contributor_workspace_id, layer,
		       provider, model, avoided_cogs_usd, minted_amount,
		       avoided_cogs_usd - (minted_amount::numeric / 1000000.0) AS margin_usd, created_at, status
		FROM pool_royalty_mints`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}

	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Millisecond) // due ~immediately, for the sweep

	// 1. HELD-CREDIT AT MINT: serve mints to held, nothing spendable, claim
	//    row status='held' with a finalize_after, and the ledger row is the
	//    UNCOUNTED held type — so GetTotalSupply must NOT see it yet.
	h := ServedHit{
		RequestID: "req-hold-1", RequesterWorkspace: "wsB", ContributorWorkspace: "wsA",
		Layer: "exact", EntryID: "e1", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0, AnswerSHA256: SHA256Hex([]byte("a")), PromptSHA256: SHA256Hex([]byte("p")),
	}
	res, err := m.MintServedHit(ctx, h)
	if err != nil || !res.Minted {
		t.Fatalf("mint: res=%+v err=%v", res, err)
	}
	var bal, held int64
	var status string
	var hasWindow bool
	if err := pool.QueryRow(ctx, `SELECT balance, held_balance FROM lens_token_balances WHERE workspace_id='wsA'`).Scan(&bal, &held); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, finalize_after IS NOT NULL FROM pool_royalty_mints WHERE request_id='req-hold-1'`).Scan(&status, &hasWindow); err != nil {
		t.Fatal(err)
	}
	if bal != 0 || held != micro(1) || status != "held" || !hasWindow {
		t.Fatalf("after mint: bal=%v held=%v status=%q window=%v — want 0/1.0/held/true", bal, held, status, hasWindow)
	}
	supply, err := ledger.GetTotalSupply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if supply != 0 {
		t.Fatalf("HELD mint must NOT count toward supply; got %v", supply)
	}

	// 2. CONCURRENT DOUBLE-FINALIZE IMPOSSIBLE: two sweepers race the same
	//    due row; the CAS lets exactly one settle it.
	time.Sleep(5 * time.Millisecond) // ensure finalize_after has passed
	s1 := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints")
	s2 := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints")
	type out struct{ n int }
	ch := make(chan out, 2)
	for _, sw := range []*FinalizeSweeper{s1, s2} {
		go func(sw *FinalizeSweeper) {
			n, _ := sw.RunOnce(ctx)
			ch <- out{n}
		}(sw)
	}
	total := (<-ch).n + (<-ch).n
	if total != 1 {
		t.Fatalf("concurrent sweeps finalized %d times, want exactly 1 (CAS guard)", total)
	}

	// 3. SUPPLY AT FINALIZE: now the counted TypePoolRoyalty row exists.
	if err := pool.QueryRow(ctx, `SELECT balance, held_balance FROM lens_token_balances WHERE workspace_id='wsA'`).Scan(&bal, &held); err != nil {
		t.Fatal(err)
	}
	if bal != micro(1) || held != 0 {
		t.Fatalf("after finalize: bal=%v held=%v, want 1.0/0 (held → spendable, conserved)", bal, held)
	}
	supply, _ = ledger.GetTotalSupply(ctx)
	if supply != micro(1) {
		t.Fatalf("finalized mint must count toward supply; got %v want micro(1) µLENS", supply)
	}

	// 4. SWEEPER NOT FLAG-GATED: a committed held row finalizes even with
	//    minting OFF (the sweeper has no flag — prove by minting with an
	//    on-flag minter, then sweeping; the sweeper object never sees the
	//    flag at all, which is the design: only the MINT is gated).
	h2 := h
	h2.RequestID = "req-hold-2"
	if _, err := m.MintServedHit(ctx, h2); err != nil {
		t.Fatal(err)
	}
	// flag now OFF — a fresh minter that refuses to mint... and the sweeper still settles the pre-existing row.
	off := NewMinter(pool, ledger, 0.5, func() bool { return false })
	if res, _ := off.MintServedHit(ctx, h2); res.Minted {
		t.Fatal("minting-off minter must not mint")
	}
	time.Sleep(5 * time.Millisecond)
	if n, err := s1.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("sweeper must finalize the committed held row regardless of the minting flag; n=%d err=%v", n, err)
	}

	// 5. REVOKE: burn-from-held does NOT decrease circulating supply (the
	//    held LENS never entered it). Mint a third held row, revoke it.
	h3 := h
	h3.RequestID = "req-hold-3"
	if _, err := m.MintServedHit(ctx, h3); err != nil {
		t.Fatal(err)
	}
	circBefore, _ := ledger.GetCirculatingSupply(ctx)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE pool_royalty_mints SET status='revoked' WHERE request_id='req-hold-3' AND status='held'`); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RevokeHeldTx(ctx, tx, "wsA", micro(1), "revoked: test", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	circAfter, _ := ledger.GetCirculatingSupply(ctx)
	if circBefore != circAfter {
		t.Fatalf("revoke must not change circulating supply: before=%v after=%v", circBefore, circAfter)
	}
	if err := pool.QueryRow(ctx, `SELECT held_balance FROM lens_token_balances WHERE workspace_id='wsA'`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 0 {
		t.Fatalf("revoked held must be burned; held=%v want 0", held)
	}

	// 6. REALIZED MARGIN IS STATUS-AWARE: of the three mints, two finalized
	//    and one was revoked — the margin surface must count exactly the two
	//    FINAL rows (held would be pending; revoked is fraudulent
	//    attribution and must never inflate realized margin).
	mr := NewMarginReader(pool)
	sum, err := mr.MarginSummary(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Mints != 2 {
		t.Fatalf("realized margin must count FINAL rows only: mints=%d want 2 (req-hold-1, req-hold-2; revoked req-hold-3 excluded)", sum.Mints)
	}
	if want := 2 * (2.0 - 1.0); sum.MarginUSD != want {
		t.Fatalf("realized margin=%v want %v", sum.MarginUSD, want)
	}
}

// PER-ENTRY CAP — common-case exactness under concurrency (real PG, -race).
// N concurrent serves of ONE entry from the SAME contributor against a cap of
// K → exactly K mint. They serialize on the shared contributor-balance row
// (CreditHeldTx's FOR UPDATE), so the after-credit entry COUNT is exact —
// identical mechanism to per-pair. (The churn-straddle over-count is the
// accepted residual and is NOT asserted as exact — see the entryCountSQL
// comment; option (a).)
func TestEntryCapExactness_ConcurrentSameContributor_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG entry-cap exactness test")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 25
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	entryCapResetSchema(t, pool, ctx)

	const capK = 5
	const concurrent = 25
	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetCap(0, time.Hour) // per-pair OFF — isolate the per-entry cap
	m.SetEntryCap(capK, time.Hour)

	var wg sync.WaitGroup
	results := make([]Result, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := ServedHit{
				RequestID:          fmt.Sprintf("entry-race-%02d", i),
				RequesterWorkspace: "wsB", ContributorWorkspace: "wsA",
				Layer: "exact", EntryID: "the-one-entry", Provider: "openai", Model: "gpt-4o",
				AvoidedCOGSUSD: 2.0, AnswerSHA256: SHA256Hex([]byte("a")), PromptSHA256: SHA256Hex([]byte("p")),
			}
			results[i], _ = m.MintServedHit(ctx, h)
		}(i)
	}
	wg.Wait()
	var minted, capped int
	for _, r := range results {
		if r.Minted {
			minted++
		} else if r.Capped {
			capped++
			if r.CapReason != "per_entry" {
				t.Errorf("capped reason = %q, want per_entry", r.CapReason)
			}
		}
	}
	if minted != capK || capped != concurrent-capK {
		t.Fatalf("EXACTNESS: minted=%d capped=%d, want %d/%d (common-case serializes on one owner row)", minted, capped, capK, concurrent-capK)
	}
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pool_royalty_mints WHERE entry_id='the-one-entry'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != capK {
		t.Fatalf("claim rows for entry = %d, want exactly %d", rows, capK)
	}
}

// REVOKED COUNTS TOWARD THE ENTRY CAP — the don't-refund-on-revoke property
// (the critical correctness test for this stage). Seed cap-worth of mints,
// REVOKE some, then assert a further mint is STILL capped: revoked rows still
// consume the entry's budget, so revocation can't reopen the exposure.
func TestEntryCap_RevokedStillCounts_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	entryCapResetSchema(t, pool, ctx)

	const capK = 5
	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetCap(0, time.Hour)
	m.SetEntryCap(capK, time.Hour)

	// Mint exactly capK on the entry (across two contributors, simulating churn).
	for i := 0; i < capK; i++ {
		contrib := "wsA"
		if i >= 2 {
			contrib = "wsCHURN" // ownership churned mid-window
		}
		h := ServedHit{
			RequestID: fmt.Sprintf("rev-%d", i), RequesterWorkspace: "wsB", ContributorWorkspace: contrib,
			Layer: "semantic", EntryID: "e-rev", Provider: "openai", Model: "gpt-4o",
			AvoidedCOGSUSD: 2.0, AnswerSHA256: SHA256Hex([]byte("a")), PromptSHA256: SHA256Hex([]byte("p")),
		}
		if res, err := m.MintServedHit(ctx, h); err != nil || !res.Minted {
			t.Fatalf("seed mint %d: res=%+v err=%v", i, res, err)
		}
	}
	// Revoke 3 of them (status='revoked' + burn from held).
	for i := 0; i < 3; i++ {
		tx, _ := pool.Begin(ctx)
		if _, err := tx.Exec(ctx, `UPDATE pool_royalty_mints SET status='revoked' WHERE request_id=$1`, fmt.Sprintf("rev-%d", i)); err != nil {
			t.Fatal(err)
		}
		_ = tx.Commit(ctx)
	}
	// A further serve must STILL be capped — revoked rows still consume budget.
	h := ServedHit{
		RequestID: "rev-after", RequesterWorkspace: "wsB", ContributorWorkspace: "wsA",
		Layer: "semantic", EntryID: "e-rev", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0, AnswerSHA256: SHA256Hex([]byte("a")), PromptSHA256: SHA256Hex([]byte("p")),
	}
	res, err := m.MintServedHit(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Capped || res.CapReason != "per_entry" {
		t.Fatalf("res=%+v — revoked mints MUST still count toward the entry cap (no budget refund on revoke)", res)
	}
	var total int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pool_royalty_mints WHERE entry_id='e-rev'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != capK { // the rev-after attempt rolled back; capK committed (3 revoked + 2 active)
		t.Fatalf("entry rows = %d, want %d (revoked rows remain, consuming budget)", total, capK)
	}
}

func entryCapResetSchema(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	for _, ddl := range []string{
		`DROP VIEW IF EXISTS pool_royalty_margin`,
		`DROP TABLE IF EXISTS pool_royalty_mints`,
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0, held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL, amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE pool_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE, requester_workspace_id TEXT NOT NULL, contributor_workspace_id TEXT NOT NULL, layer TEXT NOT NULL, entry_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', similarity DOUBLE PRECISION NOT NULL DEFAULT 0, avoided_cogs_usd DOUBLE PRECISION NOT NULL DEFAULT 0, minted_amount BIGINT NOT NULL DEFAULT 0, answer_sha256 TEXT NOT NULL DEFAULT '', prompt_sha256 TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'final', finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE INDEX IF NOT EXISTS idx_pool_royalty_mints_entry ON pool_royalty_mints (entry_id, created_at)`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
}
