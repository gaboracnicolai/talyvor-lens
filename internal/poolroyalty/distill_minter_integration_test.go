package poolroyalty

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
)

// distill_minter_integration_test.go — L2/S4 PR3 money-path proofs on real PG.
// Every assertion is on the LEDGER (held_balance / GetTotalSupply), not just the
// claim table. Gated on LENS_TEST_DATABASE_URL (CI's real-PG job).
//
// Inline minimal schema (no 0055 append-only triggers — these throwaway tables
// reset per test): the held ledger, the distill basis (source) + mints (claim),
// and minimal workspaces / lxc_purchases so earnverify.MayEarn (the U6 floor) is
// real. The verifier is wired UNCONDITIONALLY (as production does), so an
// unverified owner cannot receive a royalty.

func distillMintHarness(t *testing.T) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG distill mint test")
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
		// Drop dependents FIRST: distill_royalty_margin (PR4 view) depends on
		// distill_royalty_mints, so the table drop below errors (2BP01) if it
		// pre-exists — including a leftover in a reused CI database. Ordering-
		// independent: every harness reset clears it before the table drop.
		`DROP VIEW IF EXISTS distill_royalty_margin`,
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`DROP TABLE IF EXISTS distill_royalty_mints`,
		`DROP TABLE IF EXISTS distill_royalty_basis`,
		`DROP TABLE IF EXISTS workspaces`,
		`DROP TABLE IF EXISTS lxc_purchases`,
		`CREATE TABLE lens_token_balances (
			workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0,
			lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (
			id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL,
			type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
			metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE distill_royalty_basis (
			owner_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL,
			content_hash TEXT NOT NULL, avoided_cogs_usd DOUBLE PRECISION NOT NULL,
			vision_model TEXT NOT NULL, vision_input_tokens INTEGER NOT NULL,
			vision_output_tokens INTEGER NOT NULL, captured_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (owner_workspace_id, requester_workspace_id, content_hash))`,
		`CREATE TABLE distill_royalty_mints (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE,
			contributor_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL,
			content_hash TEXT NOT NULL, avoided_cogs_usd DOUBLE PRECISION NOT NULL,
			minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held',
			finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL)`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New()) // U6 floor — wired unconditionally, as production
	return pool, ledger
}

// seedBasis inserts one pinned avoided-COGS basis row (the PR2 record the mint
// reads). avoided is the PINNED snapshot; minted must be share × this.
func seedBasis(t *testing.T, pool *pgxpool.Pool, owner, requester, hash string, avoided float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO distill_royalty_basis
		   (owner_workspace_id, requester_workspace_id, content_hash, avoided_cogs_usd, vision_model, vision_input_tokens, vision_output_tokens)
		 VALUES ($1,$2,$3,$4,'gpt-4o-mini',500,20)`, owner, requester, hash, avoided); err != nil {
		t.Fatalf("seed basis: %v", err)
	}
}

func verifyWorkspace(t *testing.T, pool *pgxpool.Pool, ws string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, earn_verified) VALUES ($1, true)
		 ON CONFLICT (id) DO UPDATE SET earn_verified = true`, ws); err != nil {
		t.Fatalf("verify workspace: %v", err)
	}
}

func balances(t *testing.T, pool *pgxpool.Pool, ws string) (bal, held int64) {
	t.Helper()
	// No row yet ⇒ zero balances (the ledger lazily creates the row on first credit).
	_ = pool.QueryRow(context.Background(),
		`SELECT balance, held_balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&bal, &held)
	return bal, held
}

func mintRowCount(t *testing.T, pool *pgxpool.Pool, owner string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM distill_royalty_mints WHERE contributor_workspace_id=$1`, owner).Scan(&n); err != nil {
		t.Fatalf("count mints: %v", err)
	}
	return n
}

// (a) Flag-off → ZERO: basis rows present, RunOnce no-ops → no claim row, no held
// credit, no ledger row.
func TestDistillMint_FlagOff_ZeroMint_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "h1", 2.0)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return false }) // flag OFF
	n, err := m.RunOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("flag-off RunOnce: n=%d err=%v, want 0/nil", n, err)
	}
	if c := mintRowCount(t, pool, "wsA"); c != 0 {
		t.Fatalf("flag-off must write NO claim row, got %d", c)
	}
	if _, held := balances(t, pool, "wsA"); held != 0 {
		t.Fatalf("flag-off must credit nothing, held=%v", held)
	}
	if supply, _ := ledger.GetTotalSupply(ctx); supply != 0 {
		t.Fatalf("flag-off must mint nothing into supply, got %v", supply)
	}
}

// (b) Flag-on + verified owner → minted == s × pinned basis EXACTLY; HELD (not
// spendable); supply UNCHANGED while held.
func TestDistillMint_VerifiedOwner_ExactAmount_HeldNotSupply_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	const avoided = 2.0
	seedBasis(t, pool, "wsA", "wsB", "h1", avoided)

	supplyBefore, _ := ledger.GetTotalSupply(ctx)
	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	n, err := m.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("RunOnce: n=%d err=%v, want 1/nil", n, err)
	}

	// minted_amount EXACTLY = s × pinned basis × peg (0.5 × $2 × 10 LENS/$ = 10 LENS),
	// traced to the stored basis snapshot — assert minted == 0.5 × basis.avoided_cogs_usd × LENSPerUSD.
	var minted int64
	var basisCogs float64
	var status, reqID string
	if err := pool.QueryRow(ctx,
		`SELECT m.minted_amount, m.status, m.request_id, b.avoided_cogs_usd
		 FROM distill_royalty_mints m JOIN distill_royalty_basis b
		   ON b.owner_workspace_id=m.contributor_workspace_id AND b.requester_workspace_id=m.requester_workspace_id AND b.content_hash=m.content_hash
		 WHERE m.contributor_workspace_id='wsA'`).Scan(&minted, &status, &reqID, &basisCogs); err != nil {
		t.Fatal(err)
	}
	if minted != microFloorLENS(0.5*basisCogs*economy.LENSPerUSD) {
		t.Fatalf("minted_amount = %v, want exactly 0.5 × pinned basis $%v × peg = %v", minted, basisCogs, microFloorLENS(0.5*basisCogs*economy.LENSPerUSD))
	}
	if minted != micro(10) {
		t.Fatalf("minted_amount = %v, want 10 LENS ($1 of value at the peg)", minted)
	}
	if status != "held" {
		t.Fatalf("status = %q, want held", status)
	}
	if want := SHA256Hex([]byte("wsA:wsB:h1")); reqID != want {
		t.Fatalf("request_id = %q, want SHA256Hex(owner:requester:hash) = %q", reqID, want)
	}
	// Credit is HELD, not spendable.
	bal, held := balances(t, pool, "wsA")
	if bal != 0 || held != micro(10) {
		t.Fatalf("after mint: bal=%v held=%v, want 0/1.0 (held, not spendable)", bal, held)
	}
	// Supply UNCHANGED while held (counts only at finalize).
	if supply, _ := ledger.GetTotalSupply(ctx); supply != supplyBefore {
		t.Fatalf("HELD mint must NOT change supply: before=%v after=%v", supplyBefore, supply)
	}
}

// (c) Exactly-once: run the sweeper twice + a same-relationship re-attempt → ONE
// claim row AND ONE held credit. The conflict path must NOT credit.
func TestDistillMint_ExactlyOnce_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "h1", 2.0)
	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })

	if n, err := m.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("first RunOnce: n=%d err=%v, want 1", n, err)
	}
	// Second sweep: the relationship is now claimed → the LEFT JOIN excludes it →
	// nothing to mint. And a forced third RunOnce can't double-credit either.
	if n, err := m.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second RunOnce must mint nothing (already claimed): n=%d err=%v", n, err)
	}
	if n, err := m.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("third RunOnce must still mint nothing: n=%d err=%v", n, err)
	}
	if c := mintRowCount(t, pool, "wsA"); c != 1 {
		t.Fatalf("exactly-once: %d claim rows, want 1", c)
	}
	if _, held := balances(t, pool, "wsA"); held != micro(10) {
		t.Fatalf("exactly-once: held=%v, want 10 LENS (a conflict must NOT credit a second time)", held)
	}
}

// (d) Sybil → ZERO then re-eligible: unverified owner → verifyEarn rolls back →
// NO claim row, no credit; then verify + re-run → mints exactly once (floor
// DELAYED, didn't forfeit).
func TestDistillMint_UnverifiedOwner_ZeroThenReeligible_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	// wsA is NOT verified (no workspaces row, no lxc_purchase).
	seedBasis(t, pool, "wsA", "wsB", "h1", 2.0)
	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })

	n, err := m.RunOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("unverified owner: RunOnce n=%d err=%v, want 0/nil (mint rolled back)", n, err)
	}
	if c := mintRowCount(t, pool, "wsA"); c != 0 {
		t.Fatalf("unverified-A rollback must leave NO claim row (else permanent forfeit), got %d", c)
	}
	if _, held := balances(t, pool, "wsA"); held != 0 {
		t.Fatalf("unverified owner credited %v, want 0", held)
	}

	// Now A verifies → the previously-skipped relationship mints exactly once.
	verifyWorkspace(t, pool, "wsA")
	n, err = m.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("after verify, the delayed relationship must mint once: n=%d err=%v", n, err)
	}
	if c := mintRowCount(t, pool, "wsA"); c != 1 {
		t.Fatalf("after verify: %d claim rows, want 1", c)
	}
	if _, held := balances(t, pool, "wsA"); held != micro(10) {
		t.Fatalf("after verify: held=%v, want 10 LENS (floor delayed, did not forfeit)", held)
	}
}

// (e) Killswitch → ZERO: a minter whose flag closure reflects the economy-off
// force-off mints nothing (the config force-off of PoolRoyaltyMintingEnabled is
// asserted separately in config tests; here we prove the minter honors it).
func TestDistillMint_KillswitchForceOff_ZeroMint_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "h1", 2.0)

	// Economy off ⇒ cfg.PoolRoyaltyMintingEnabled is force-set false ⇒ the closure
	// returns false ⇒ the minter no-ops, exactly like flag-off.
	mintFlag := false // stands for the force-off'd cfg.PoolRoyaltyMintingEnabled
	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return mintFlag })
	n, err := m.RunOnce(ctx)
	if err != nil || n != 0 {
		t.Fatalf("killswitch (flag force-off): n=%d err=%v, want 0", n, err)
	}
	if c := mintRowCount(t, pool, "wsA"); c != 0 {
		t.Fatalf("killswitch must write no claim row, got %d", c)
	}
	if supply, _ := ledger.GetTotalSupply(ctx); supply != 0 {
		t.Fatalf("killswitch must mint nothing, supply=%v", supply)
	}
}

// (f) Finalize: after the holdback, the distill finalize sweeper settles the held
// row → held→spendable + the counted TypePoolRoyalty row → supply increases at
// FINALIZE, not at held.
func TestDistillMint_FinalizeCountsSupplyAtFinalize_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "h1", 2.0)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Millisecond) // due ~immediately
	supplyBefore, _ := ledger.GetTotalSupply(ctx)
	if n, err := m.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("mint: n=%d err=%v", n, err)
	}
	// Held: supply still unchanged.
	if supply, _ := ledger.GetTotalSupply(ctx); supply != supplyBefore {
		t.Fatalf("held mint changed supply early: before=%v after=%v", supplyBefore, supply)
	}

	time.Sleep(5 * time.Millisecond) // finalize_after elapses
	sw := NewFinalizeSweeper(pool, ledger, "distill_royalty_mints")
	got, err := sw.RunOnce(ctx)
	if err != nil || got != 1 {
		t.Fatalf("finalize sweep: finalized=%d err=%v, want 1", got, err)
	}
	// held→spendable, status final.
	bal, held := balances(t, pool, "wsA")
	if bal != micro(10) || held != 0 {
		t.Fatalf("after finalize: bal=%v held=%v, want 10/0 (spendable)", bal, held)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM distill_royalty_mints WHERE contributor_workspace_id='wsA'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "final" {
		t.Fatalf("status = %q, want final", status)
	}
	// Supply INCREASES at finalize (the counted TypePoolRoyalty row).
	if supply, _ := ledger.GetTotalSupply(ctx); supply != supplyBefore+micro(10) {
		t.Fatalf("supply must increase by micro(10) at FINALIZE: before=%v after=%v", supplyBefore, supply)
	}
}
