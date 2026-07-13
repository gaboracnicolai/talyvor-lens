package provenance_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/outputverify"
	"github.com/talyvor/lens/internal/provenance"
	"github.com/talyvor/lens/migrations"
)

var bondMigrateOnce sync.Once

func bondTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG provenance test")
	}
	ctx := context.Background()
	bondMigrateOnce.Do(func() {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for migrate: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	})
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newManager(pool *pgxpool.Pool) *provenance.BondManager {
	return provenance.NewBondManager(pool, mining.NewLedgerStore(pool), 72*time.Hour, 10000)
}

// ── seed helpers ────────────────────────────────────────────────────────────

func seedBalance(t *testing.T, pool *pgxpool.Pool, ws string, amount int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO lens_token_balances (workspace_id, balance) VALUES ($1,$2)
		 ON CONFLICT (workspace_id) DO UPDATE SET balance=$2, locked_balance=0`, ws, amount)
	if err != nil {
		t.Fatalf("seed balance: %v", err)
	}
}

// seedOwnedOutput records a k4_output_verdicts row so `ws` is the recognised OWNER/producer of outputID.
func seedOwnedOutput(t *testing.T, pool *pgxpool.Pool, ws, outputID string) {
	t.Helper()
	_, err := outputverify.NewWriter(pool).Record(context.Background(), outputverify.VerdictRecord{
		OutputID: outputID, WorkspaceID: ws, Model: "m",
		Verdict: outputverify.VerdictUnverifiable, ConstraintKind: outputverify.KindNone,
		PromptSHA256: outputverify.Sha256Hex([]byte("p")), ResponseSHA256: outputverify.Sha256Hex([]byte("r")),
	})
	if err != nil {
		t.Fatalf("seed owned output: %v", err)
	}
}

// seedMechVerdict raw-inserts a mechanical verdict. Raw (not the ownership-bound writer) so tests can forge
// the adversarial "workspace B reported on A's output" row the bond path must still refuse.
func seedMechVerdict(t *testing.T, pool *pgxpool.Pool, ws, outputID, verdict, source string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO k4_mechanical_verdicts (output_id, workspace_id, verdict, exit_code, verdict_source)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, outputID, ws, verdict, 1, source)
	if err != nil {
		t.Fatalf("seed mech verdict: %v", err)
	}
}

func pushDeadlinePast(t *testing.T, pool *pgxpool.Pool, bondID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE provenance_bonds SET appeal_deadline = now() - interval '1 hour' WHERE bond_id=$1`, bondID); err != nil {
		t.Fatalf("push deadline: %v", err)
	}
}

func balanceOf(t *testing.T, pool *pgxpool.Pool, ws string) (balance, locked int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT balance, locked_balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&balance, &locked); err != nil {
		t.Fatalf("balanceOf: %v", err)
	}
	return balance, locked
}

func totalSupply(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var s int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(balance + locked_balance),0) FROM lens_token_balances`).Scan(&s); err != nil {
		t.Fatalf("totalSupply: %v", err)
	}
	return s
}

// creditsToOthers counts positive-amount ledger rows to any workspace other than `self` — a slash must
// produce ZERO of these (the burn is paid to NOBODY).
func creditsToOthers(t *testing.T, pool *pgxpool.Pool, self string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM lens_token_ledger WHERE workspace_id <> $1 AND amount > 0`, self).Scan(&n); err != nil {
		t.Fatalf("creditsToOthers: %v", err)
	}
	return n
}

// ── MONEY-SAFETY ────────────────────────────────────────────────────────────

// THE most important assertion in the file. A slash BURNS: supply drops by exactly the burn, the bonder's
// available balance is untouched, and NOBODY else is credited a single µLENS.
func TestBond_Slash_BurnsNotBounty(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, B, oid = "ws-burn-A", "ws-burn-B", "oid-burn-1"
	seedBalance(t, pool, A, 5_000_000)
	seedBalance(t, pool, B, 2_000_000) // an innocent bystander
	seedOwnedOutput(t, pool, A, oid)
	seedMechVerdict(t, pool, A, oid, outputverify.MechCompileFailed, outputverify.SourceSelfReported)

	bondID, created, err := m.CreateBond(ctx, A, oid, 1_000_000)
	if err != nil || !created {
		t.Fatalf("CreateBond: created=%v err=%v", created, err)
	}
	// After locking: A has 4_000_000 available + 1_000_000 locked.
	if bal, lk := balanceOf(t, pool, A); bal != 4_000_000 || lk != 1_000_000 {
		t.Fatalf("after lock: bal=%d locked=%d, want 4_000_000/1_000_000", bal, lk)
	}
	supplyBefore := totalSupply(t, pool)
	bBalBefore, bLockBefore := balanceOf(t, pool, B)

	pushDeadlinePast(t, pool, bondID)
	outcome, err := m.SettleBond(ctx, bondID)
	if err != nil || outcome != "slashed" {
		t.Fatalf("settle: outcome=%q err=%v, want slashed", outcome, err)
	}

	// BURN: A's locked collateral is gone; A's AVAILABLE balance is untouched.
	if bal, lk := balanceOf(t, pool, A); bal != 4_000_000 || lk != 0 {
		t.Errorf("after slash: bal=%d locked=%d, want 4_000_000/0 (locked burned, available untouched)", bal, lk)
	}
	// SUPPLY REDUCED by exactly the bond.
	if got := totalSupply(t, pool); got != supplyBefore-1_000_000 {
		t.Errorf("supply must drop by the burn: got %d, want %d", got, supplyBefore-1_000_000)
	}
	// NOT A BOUNTY: the bystander B is untouched, and NOBODY but A has any ledger movement credited to them.
	if bal, lk := balanceOf(t, pool, B); bal != bBalBefore || lk != bLockBefore {
		t.Errorf("bystander B must be untouched: bal %d→%d locked %d→%d", bBalBefore, bal, bLockBefore, lk)
	}
	if n := creditsToOthers(t, pool, A); n != 0 {
		t.Errorf("BURN-NOT-BOUNTY: the slashed value went to NOBODY, but %d credit(s) to other workspaces exist", n)
	}
}

// NO-DOUBLE-SLASH under real concurrency: two goroutines settle one slashable bond → exactly ONE burn.
func TestBond_NoDoubleSlash_Race(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, oid = "ws-race-A", "oid-race-1"
	seedBalance(t, pool, A, 10_000_000)
	seedOwnedOutput(t, pool, A, oid)
	seedMechVerdict(t, pool, A, oid, outputverify.MechTestsFailed, outputverify.SourceSelfReported)
	bondID, _, err := m.CreateBond(ctx, A, oid, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	pushDeadlinePast(t, pool, bondID)

	var wg sync.WaitGroup
	outcomes := make([]string, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i], _ = m.SettleBond(ctx, bondID)
		}(i)
	}
	wg.Wait()

	slashed := 0
	for _, o := range outcomes {
		if o == "slashed" {
			slashed++
		}
	}
	if slashed != 1 {
		t.Errorf("exactly ONE goroutine may slash; got %d (outcomes=%v)", slashed, outcomes)
	}
	// Exactly ONE burn ledger row.
	var burns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lens_token_ledger WHERE workspace_id=$1 AND type='povi_stake_slash'`, A).Scan(&burns); err != nil {
		t.Fatal(err)
	}
	if burns != 1 {
		t.Errorf("exactly one burn ledger delta; got %d", burns)
	}
	if _, lk := balanceOf(t, pool, A); lk != 0 {
		t.Errorf("locked must be burned exactly once → 0; got %d", lk)
	}
}

// NO-REPLAY: settling an already-slashed bond is a CAS no-op — ledger unchanged, slash_key stable.
func TestBond_NoReplay(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, oid = "ws-replay-A", "oid-replay-1"
	seedBalance(t, pool, A, 10_000_000)
	seedOwnedOutput(t, pool, A, oid)
	seedMechVerdict(t, pool, A, oid, outputverify.MechCompileFailed, outputverify.SourceSelfReported)
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	pushDeadlinePast(t, pool, bondID)

	if o, _ := m.SettleBond(ctx, bondID); o != "slashed" {
		t.Fatalf("first settle: %q want slashed", o)
	}
	_, lk1 := balanceOf(t, pool, A)
	o2, _ := m.SettleBond(ctx, bondID)
	if o2 != "settled_already" {
		t.Errorf("replay must be a no-op; got %q", o2)
	}
	if _, lk2 := balanceOf(t, pool, A); lk2 != lk1 {
		t.Errorf("replay changed locked balance %d→%d", lk1, lk2)
	}
	var burns int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM lens_token_ledger WHERE workspace_id=$1 AND type='povi_stake_slash'`, A).Scan(&burns)
	if burns != 1 {
		t.Errorf("replay must not add a burn; got %d burns", burns)
	}
}

// CANNOT-EXCEED: a partial-bps slash burns strictly less than the bond; locked never goes negative.
func TestBond_SlashCannotExceedBond_PartialBps(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := provenance.NewBondManager(pool, mining.NewLedgerStore(pool), 72*time.Hour, 2500) // 25%
	const A, oid = "ws-partial-A", "oid-partial-1"
	seedBalance(t, pool, A, 10_000_000)
	seedOwnedOutput(t, pool, A, oid)
	seedMechVerdict(t, pool, A, oid, outputverify.MechCompileFailed, outputverify.SourceSelfReported)
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	pushDeadlinePast(t, pool, bondID)
	if o, err := m.SettleBond(ctx, bondID); o != "slashed" || err != nil {
		t.Fatalf("settle: %q %v", o, err)
	}
	// 25% of 1_000_000 = 250_000 burned; 750_000 remains LOCKED (never released early, never over-burned).
	if bal, lk := balanceOf(t, pool, A); bal != 9_000_000 || lk != 750_000 {
		t.Errorf("partial slash: bal=%d locked=%d, want 9_000_000/750_000", bal, lk)
	}
}

// ── AUTHORIZATION — each must be PROVEN impossible ───────────────────────────

// A self-reported PASS cannot slash — at the deadline the bond RELEASES intact.
func TestBond_PassCannotSlash(t *testing.T) {
	assertReleasesDespiteVerdict(t, outputverify.MechCompiled, outputverify.SourceSelfReported)
}

// An unknown/forged verdict_source can never even be RECORDED (migration 0087 CHECK), so it can never reach
// the bond slash path — a stronger guarantee than "it doesn't slash". self_reported + talyvor_verified are
// the ONLY sources that exist.
func TestBond_UnknownSourceCannotBeRecorded(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	_, err := pool.Exec(ctx,
		`INSERT INTO k4_mechanical_verdicts (output_id, workspace_id, verdict, exit_code, verdict_source)
		 VALUES ('oid-unk-src','ws','compile_failed',1,'attested')`)
	if err == nil {
		t.Error("an unknown verdict_source must be rejected by the 0087 CHECK — it can never feed a slash")
	}
}

func assertReleasesDespiteVerdict(t *testing.T, verdict, source string) {
	t.Helper()
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	ws := "ws-auth-" + verdict + "-" + source
	oid := "oid-auth-" + verdict + "-" + source
	seedBalance(t, pool, ws, 5_000_000)
	seedOwnedOutput(t, pool, ws, oid)
	seedMechVerdict(t, pool, ws, oid, verdict, source)
	bondID, _, _ := m.CreateBond(ctx, ws, oid, 1_000_000)
	pushDeadlinePast(t, pool, bondID)
	o, err := m.SettleBond(ctx, bondID)
	if err != nil {
		t.Fatal(err)
	}
	if o != "released" {
		t.Errorf("verdict=%q source=%q must NOT slash; outcome=%q want released", verdict, source, o)
	}
	if bal, lk := balanceOf(t, pool, ws); bal != 5_000_000 || lk != 0 {
		t.Errorf("released bond returns full collateral: bal=%d locked=%d, want 5_000_000/0", bal, lk)
	}
}

// NO verdict at all → the bond releases (nothing can slash it).
func TestBond_NoVerdictCannotSlash(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, oid = "ws-noverdict-A", "oid-noverdict-1"
	seedBalance(t, pool, A, 5_000_000)
	seedOwnedOutput(t, pool, A, oid) // owned, but NO mechanical verdict ever recorded
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	pushDeadlinePast(t, pool, bondID)
	if o, _ := m.SettleBond(ctx, bondID); o != "released" {
		t.Errorf("no verdict → must release, not slash; got %q", o)
	}
}

// A slash-usable verdict for a DIFFERENT output cannot slash THIS bond.
func TestBond_DifferentOutputCannotSlash(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, oid, otherOID = "ws-diff-A", "oid-diff-bonded", "oid-diff-other"
	seedBalance(t, pool, A, 5_000_000)
	seedOwnedOutput(t, pool, A, oid)
	seedOwnedOutput(t, pool, A, otherOID)
	seedMechVerdict(t, pool, A, otherOID, outputverify.MechCompileFailed, outputverify.SourceSelfReported) // on the OTHER output
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	pushDeadlinePast(t, pool, bondID)
	if o, _ := m.SettleBond(ctx, bondID); o != "released" {
		t.Errorf("a verdict on a different output must not slash this bond; got %q", o)
	}
}

// WORKSPACE B CANNOT cause A's bond to be slashed — even a forged verdict row on A's output attributed to B
// is refused (the bond path requires the verdict be by the BONDER). End-to-end ownership re-proof.
func TestBond_WorkspaceBCannotSlashA(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, B, oid = "ws-A-victim", "ws-B-attacker", "oid-victim-1"
	seedBalance(t, pool, A, 5_000_000)
	seedOwnedOutput(t, pool, A, oid) // A owns the output
	// B forges a failing verdict on A's output (0085 would reject this at the endpoint; here we insert it
	// directly to prove the BOND path defends even if such a row existed).
	seedMechVerdict(t, pool, B, oid, outputverify.MechCompileFailed, outputverify.SourceSelfReported)
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	pushDeadlinePast(t, pool, bondID)
	o, _ := m.SettleBond(ctx, bondID)
	if o != "released" {
		t.Errorf("B's verdict on A's output must NOT slash A's bond; outcome=%q want released", o)
	}
	if bal, lk := balanceOf(t, pool, A); bal != 5_000_000 || lk != 0 {
		t.Errorf("A's collateral must be returned intact: bal=%d locked=%d", bal, lk)
	}
}

// A bond whose appeal window is STILL OPEN cannot be burned — a pending slash only marks it 'appealing'.
func TestBond_AppealWindowCannotBurn(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool) // 72h window → open
	const A, oid = "ws-appeal-A", "oid-appeal-1"
	seedBalance(t, pool, A, 5_000_000)
	seedOwnedOutput(t, pool, A, oid)
	seedMechVerdict(t, pool, A, oid, outputverify.MechCompileFailed, outputverify.SourceSelfReported)
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	// deadline NOT pushed → window open.
	o, err := m.SettleBond(ctx, bondID)
	if err != nil {
		t.Fatal(err)
	}
	if o != "appealing" {
		t.Errorf("open window with a pending slash → 'appealing', not a burn; got %q", o)
	}
	// NOTHING burned — collateral still fully locked.
	if bal, lk := balanceOf(t, pool, A); bal != 4_000_000 || lk != 1_000_000 {
		t.Errorf("open window must not burn: bal=%d locked=%d, want 4_000_000/1_000_000", bal, lk)
	}
	var burns int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM lens_token_ledger WHERE workspace_id=$1 AND type='povi_stake_slash'`, A).Scan(&burns)
	if burns != 0 {
		t.Errorf("no burn may occur while the window is open; got %d", burns)
	}
}

// ── RELEASE ─────────────────────────────────────────────────────────────────

// No slash-usable verdict by the deadline → the bond releases the FULL collateral.
func TestBond_ReleasesAfterDeadline(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, oid = "ws-rel-A", "oid-rel-1"
	seedBalance(t, pool, A, 3_000_000)
	seedOwnedOutput(t, pool, A, oid)
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	pushDeadlinePast(t, pool, bondID)
	if o, _ := m.SettleBond(ctx, bondID); o != "released" {
		t.Fatalf("no verdict by deadline → released; got %q", o)
	}
	if bal, lk := balanceOf(t, pool, A); bal != 3_000_000 || lk != 0 {
		t.Errorf("release returns all collateral: bal=%d locked=%d, want 3_000_000/0", bal, lk)
	}
}

// A self-reported PASS does NOT release the bond early — only TIME (the deadline) releases it.
func TestBond_PassDoesNotReleaseEarly(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool) // window open
	const A, oid = "ws-passearly-A", "oid-passearly-1"
	seedBalance(t, pool, A, 3_000_000)
	seedOwnedOutput(t, pool, A, oid)
	seedMechVerdict(t, pool, A, oid, outputverify.MechTestsPassed, outputverify.SourceSelfReported)
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	// window still open + a PASS present.
	o, _ := m.SettleBond(ctx, bondID)
	if o != "pending" {
		t.Errorf("a PASS during the window must NOT release early; got %q (want pending)", o)
	}
	if bal, lk := balanceOf(t, pool, A); bal != 2_000_000 || lk != 1_000_000 {
		t.Errorf("collateral must stay locked: bal=%d locked=%d, want 2_000_000/1_000_000", bal, lk)
	}
}

// ── CREATE ──────────────────────────────────────────────────────────────────

// CreateBond refuses an output the workspace does not own — no collateral is locked.
func TestBond_CreateRequiresOwnership(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, oid = "ws-own-A", "oid-unowned-1"
	seedBalance(t, pool, A, 5_000_000)
	// NO seedOwnedOutput → A does not own oid.
	_, created, err := m.CreateBond(ctx, A, oid, 1_000_000)
	if created || err != provenance.ErrNotOwned {
		t.Errorf("must refuse a bond on an unowned output; created=%v err=%v", created, err)
	}
	if bal, lk := balanceOf(t, pool, A); bal != 5_000_000 || lk != 0 {
		t.Errorf("no collateral may be locked on a refused bond: bal=%d locked=%d", bal, lk)
	}
}

// CreateBond is idempotent per (workspace, output) — a re-create does not double-lock.
func TestBond_CreateIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, oid = "ws-idem-A", "oid-idem-1"
	seedBalance(t, pool, A, 5_000_000)
	seedOwnedOutput(t, pool, A, oid)
	if _, created, _ := m.CreateBond(ctx, A, oid, 1_000_000); !created {
		t.Fatal("first create must lock")
	}
	if _, created, _ := m.CreateBond(ctx, A, oid, 1_000_000); created {
		t.Error("second create must be a no-op (idempotent)")
	}
	if bal, lk := balanceOf(t, pool, A); bal != 4_000_000 || lk != 1_000_000 {
		t.Errorf("exactly one lock: bal=%d locked=%d, want 4_000_000/1_000_000", bal, lk)
	}
}

// ── ATTESTED SOURCE (step 1) ─────────────────────────────────────────────────

// An ATTESTED compile failure (talyvor_verified) SLASHES the bond — end to end.
func TestBond_AttestedCompileFailed_Slashes(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, oid = "ws-att-cf", "oid-att-cf"
	seedBalance(t, pool, A, 5_000_000)
	seedOwnedOutput(t, pool, A, oid)
	seedMechVerdict(t, pool, A, oid, outputverify.MechCompileFailed, outputverify.SourceTalyvorVerified)
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	pushDeadlinePast(t, pool, bondID)
	if o, err := m.SettleBond(ctx, bondID); o != "slashed" || err != nil {
		t.Fatalf("attested compile_failed must slash; outcome=%q err=%v", o, err)
	}
	if bal, lk := balanceOf(t, pool, A); bal != 4_000_000 || lk != 0 {
		t.Errorf("after attested slash: bal=%d locked=%d, want 4_000_000/0", bal, lk)
	}
}

// An ATTESTED PASS (talyvor_verified + compiled) does NOT slash — the bond releases.
func TestBond_AttestedPass_Releases(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, oid = "ws-att-pass", "oid-att-pass"
	seedBalance(t, pool, A, 5_000_000)
	seedOwnedOutput(t, pool, A, oid)
	seedMechVerdict(t, pool, A, oid, outputverify.MechCompiled, outputverify.SourceTalyvorVerified)
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	pushDeadlinePast(t, pool, bondID)
	if o, _ := m.SettleBond(ctx, bondID); o != "released" {
		t.Errorf("an attested PASS must NOT slash; got %q want released", o)
	}
	if bal, lk := balanceOf(t, pool, A); bal != 5_000_000 || lk != 0 {
		t.Errorf("released bond returns full collateral: bal=%d locked=%d", bal, lk)
	}
}

// TWO SOURCES, ONE BURN — the subtle case. A self_reported AND a talyvor_verified compile_failed coexist for
// one output (the PK allows it). The bond must still slash EXACTLY ONCE (the status CAS + slash_key
// idempotency hold regardless of how many slash-usable verdicts exist).
func TestBond_TwoSources_OneBurn(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	m := newManager(pool)
	const A, oid = "ws-two-src", "oid-two-src"
	seedBalance(t, pool, A, 5_000_000)
	seedOwnedOutput(t, pool, A, oid)
	// BOTH slash-usable verdicts for the same output.
	seedMechVerdict(t, pool, A, oid, outputverify.MechCompileFailed, outputverify.SourceSelfReported)
	seedMechVerdict(t, pool, A, oid, outputverify.MechCompileFailed, outputverify.SourceTalyvorVerified)
	// sanity: both rows really exist.
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM k4_mechanical_verdicts WHERE output_id=$1`, oid).Scan(&n)
	if n != 2 {
		t.Fatalf("expected both sources present, got %d rows", n)
	}
	bondID, _, _ := m.CreateBond(ctx, A, oid, 1_000_000)
	pushDeadlinePast(t, pool, bondID)

	if o, _ := m.SettleBond(ctx, bondID); o != "slashed" {
		t.Fatalf("first settle must slash; got %q", o)
	}
	if o, _ := m.SettleBond(ctx, bondID); o != "settled_already" {
		t.Errorf("second settle must be a no-op with two sources present; got %q", o)
	}
	// EXACTLY ONE burn, despite two slash-usable verdicts.
	var burns int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM lens_token_ledger WHERE workspace_id=$1 AND type='povi_stake_slash'`, A).Scan(&burns)
	if burns != 1 {
		t.Errorf("two slash-usable sources must still burn ONCE; got %d burns", burns)
	}
	if bal, lk := balanceOf(t, pool, A); bal != 4_000_000 || lk != 0 {
		t.Errorf("exactly one burn: bal=%d locked=%d, want 4_000_000/0", bal, lk)
	}
}
