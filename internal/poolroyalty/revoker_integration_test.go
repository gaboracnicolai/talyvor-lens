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

// Real-PG revoke-orchestrator tests (LENS_TEST_DATABASE_URL-gated, the CI
// postgres pattern). The money correctness (held burn, status flip, supply
// uncounted) and the concurrent revoke-vs-finalize race can only be proven
// against a real engine.

func revokerTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG revoker test")
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
	entryCapResetSchema(t, pool, ctx) // reuses the cap test's full schema (balances+ledger+mints+index)
	return pool
}

// seed one HELD mint via the real minter so balances/held + the claim row are
// all consistent, and return its SERVER-DERIVED request_id (SEC-11 — no longer
// the client header; reqID/contributor vary the key so seeds don't collide).
func seedHeldMint(t *testing.T, m *Minter, ctx context.Context, reqID, contributor string, amount float64) string {
	t.Helper()
	h := ServedHit{
		RequestID: reqID, RequesterWorkspace: "wsB", ContributorWorkspace: contributor,
		Layer: "exact", EntryID: "e", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: amount * 2, // s=0.5 × (amount×2) × 10 LENS/$ peg ⇒ minted = amount × 10 LENS
		// The answer hash is a KEY input now, so vary it by (reqID, contributor)
		// to give each seed a distinct server-derived key.
		AnswerSHA256: SHA256Hex([]byte("a:" + reqID + ":" + contributor)), PromptSHA256: SHA256Hex([]byte("p")),
	}
	res, err := m.MintServedHit(ctx, h)
	if err != nil || !res.Minted {
		t.Fatalf("seed held mint %s: res=%+v err=%v", reqID, res, err)
	}
	return res.RequestID
}

// SINGLE REVOKE — money correctness: status flips held→revoked, held_balance
// burned, spendable balance + lifetime_earned UNCHANGED, a pool_royalty_revoked
// ledger row written, and supply stays uncounted (revoked not in circulating).
func TestRevoker_SingleRevoke_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Hour) // stays held; sweeper won't touch it
	rk := seedHeldMint(t, m, ctx, "rk-1", "wsA", 1.0)

	// pre: held=1, spendable=0, earned=0, supply=0 (held mint not counted)
	var bal, held, earned int64
	mustScan(t, pool, `SELECT balance, held_balance, lifetime_earned FROM lens_token_balances WHERE workspace_id='wsA'`, &bal, &held, &earned)
	if bal != 0 || held != micro(10) || earned != 0 {
		t.Fatalf("pre-revoke: bal=%v held=%v earned=%v, want 0/10/0", bal, held, earned)
	}

	r := NewRevoker(pool, ledger)
	rep, err := r.RevokeHeldMints(ctx, []string{rk})
	if err != nil || rep.Outcomes[rk] != OutcomeRevoked {
		t.Fatalf("revoke: rep=%+v err=%v", rep, err)
	}

	// post: held burned to 0; spendable + earned STILL 0 (revoke touches neither)
	mustScan(t, pool, `SELECT balance, held_balance, lifetime_earned FROM lens_token_balances WHERE workspace_id='wsA'`, &bal, &held, &earned)
	if bal != 0 || held != 0 || earned != 0 {
		t.Fatalf("post-revoke: bal=%v held=%v earned=%v, want 0/0/0 (held burned, spendable/earned untouched)", bal, held, earned)
	}
	// claim row revoked
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM pool_royalty_mints WHERE request_id=$1`, rk).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "revoked" {
		t.Fatalf("status=%q, want revoked", status)
	}
	// a pool_royalty_revoked ledger row exists
	var revRows int64
	mustScan(t, pool, `SELECT COUNT(*) FROM lens_token_ledger WHERE type='pool_royalty_revoked'`, &revRows)
	if revRows != 1 {
		t.Fatalf("pool_royalty_revoked ledger rows = %d, want 1", revRows)
	}
	// supply uncounted: circulating unaffected by the revoke (revoked not minted, not burned-from-supply)
	circ, _ := ledger.GetCirculatingSupply(ctx)
	if circ != 0 {
		t.Fatalf("circulating supply = %v, want 0 (a revoked held mint never entered supply)", circ)
	}
}

// IDEMPOTENCY end-to-end: revoke the same id twice → first revoked, second
// skipped_already_revoked; held burned EXACTLY ONCE.
func TestRevoker_Idempotent_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Hour)
	rk := seedHeldMint(t, m, ctx, "rk-idem", "wsA", 3.0)

	r := NewRevoker(pool, ledger)
	rep1, _ := r.RevokeHeldMints(ctx, []string{rk})
	rep2, _ := r.RevokeHeldMints(ctx, []string{rk})
	if rep1.Outcomes[rk] != OutcomeRevoked {
		t.Fatalf("first call: %q, want revoked", rep1.Outcomes[rk])
	}
	if rep2.Outcomes[rk] != OutcomeSkippedAlreadyRevoked {
		t.Fatalf("second call: %q, want skipped_already_revoked", rep2.Outcomes[rk])
	}
	var revRows int64
	mustScan(t, pool, `SELECT COUNT(*) FROM lens_token_ledger WHERE type='pool_royalty_revoked'`, &revRows)
	if revRows != 1 {
		t.Fatalf("held burned %d times, want EXACTLY 1 (idempotent)", revRows)
	}
}

// FINALITY GUARD end-to-end: finalize a held mint (sweeper), THEN revoke it →
// skipped_not_held, no burn. A finalized mint is NEVER revocable.
func TestRevoker_FinalizedNeverRevocable_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Millisecond)
	rk := seedHeldMint(t, m, ctx, "rk-fin", "wsA", 2.0)
	time.Sleep(5 * time.Millisecond)
	// finalize it
	sw := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints")
	if n, err := sw.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("finalize: n=%d err=%v", n, err)
	}
	// now revoke must skip
	r := NewRevoker(pool, ledger)
	rep, _ := r.RevokeHeldMints(ctx, []string{rk})
	if rep.Outcomes[rk] != OutcomeSkippedNotHeld {
		t.Fatalf("finalized mint outcome = %q, want skipped_not_held (NEVER revocable)", rep.Outcomes[rk])
	}
	var revRows int64
	mustScan(t, pool, `SELECT COUNT(*) FROM lens_token_ledger WHERE type='pool_royalty_revoked'`, &revRows)
	if revRows != 0 {
		t.Fatalf("a finalized mint must NEVER burn-revoke; rows=%d", revRows)
	}
	// it finalized → spendable=20, held=0
	var bal, held int64
	mustScan(t, pool, `SELECT balance, held_balance FROM lens_token_balances WHERE workspace_id='wsA'`, &bal, &held)
	if bal != micro(20) || held != 0 {
		t.Fatalf("finalized: bal=%v held=%v, want 20/0", bal, held)
	}
}

// THE RACE (-race, real PG): concurrent revoke + finalize-sweeper on the SAME
// held row → exactly one wins. Either it finalizes (spendable, revoke skips) or
// it revokes (held burned, finalize skips) — NEVER both, NEVER a burn without a
// successful CAS. The claim-row status='held' guard is the single serialization
// point both contend on.
func TestRevoker_ConcurrentRevokeVsFinalize_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	r := NewRevoker(pool, ledger)

	const N = 30
	finalizedCount, revokedCount := 0, 0
	for i := 0; i < N; i++ {
		req := fmt.Sprintf("race-%02d", i)
		m := NewMinter(pool, ledger, 0.5, func() bool { return true })
		m.SetHoldbackWindow(time.Millisecond)
		key := seedHeldMint(t, m, ctx, req, fmt.Sprintf("ws-%02d", i), 1.0) // server-derived key
		time.Sleep(2 * time.Millisecond)                                    // finalize_after passed → both ops are eligible

		sw := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints")
		var wg sync.WaitGroup
		var finN int
		var revOut RevokeOutcome
		wg.Add(2)
		go func() { defer wg.Done(); finN, _ = sw.RunOnce(ctx) }()
		go func() {
			defer wg.Done()
			rep, _ := r.RevokeHeldMints(ctx, []string{key})
			revOut = rep.Outcomes[key]
		}()
		wg.Wait()

		// EXACTLY ONE terminal state on the claim row.
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM pool_royalty_mints WHERE request_id=$1`, key).Scan(&status); err != nil {
			t.Fatalf("scan status for %s: %v", req, err)
		}
		switch status {
		case "final":
			finalizedCount++
			if revOut == OutcomeRevoked {
				t.Fatalf("%s: row is final but revoke claims revoked — BOTH won", req)
			}
		case "revoked":
			revokedCount++
			if revOut != OutcomeRevoked {
				t.Fatalf("%s: row revoked but revoke didn't report it (%q)", req, revOut)
			}
			_ = finN
		default:
			t.Fatalf("%s: status=%q, want final or revoked (exactly one must win)", req, status)
		}
	}

	// Global ledger invariant: every row reached exactly one terminal write,
	// and the count of revoked-burns equals the count of revoked rows (no burn
	// without a successful CAS, no double-burn).
	var finalRows, revokedRows, revBurns int64
	mustScan(t, pool, `SELECT COUNT(*) FROM pool_royalty_mints WHERE status='final'`, &finalRows)
	mustScan(t, pool, `SELECT COUNT(*) FROM pool_royalty_mints WHERE status='revoked'`, &revokedRows)
	mustScan(t, pool, `SELECT COUNT(*) FROM lens_token_ledger WHERE type='pool_royalty_revoked'`, &revBurns)
	if finalRows+revokedRows != int64(N) {
		t.Fatalf("terminal rows = %d final + %d revoked != %d", finalRows, revokedRows, N)
	}
	if revBurns != revokedRows {
		t.Fatalf("revoked-burns=%d != revoked-rows=%d — a burn without a CAS (or a double-burn) occurred", revBurns, revokedRows)
	}
	t.Logf("race outcome over %d rows: %d finalized, %d revoked (both valid; never both on one row)", N, finalizedCount, revokedCount)
}

// REVOKED STILL COUNTS TOWARD CAP — no budget refund on revoke (consistent with
// the per-entry cap decision): mint cap-worth on one entry, revoke some via the
// orchestrator, then a further mint of that entry is STILL capped.
func TestRevoker_RevokedStillCountsTowardCap_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	const capK = 5
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Hour)
	m.SetEntryCap(capK, time.Hour)

	ids := make([]string, 0, capK)
	for i := 0; i < capK; i++ {
		// Distinct requesters served the SAME entry → capK distinct server-derived
		// keys on one entry_id (SEC-11: reusing one header no longer distinguishes
		// mints — the key does). The per-entry cap counts across requesters.
		h := ServedHit{
			RequestID: fmt.Sprintf("cap-rk-%d", i), RequesterWorkspace: fmt.Sprintf("wsB-%d", i), ContributorWorkspace: "wsA",
			Layer: "exact", EntryID: "capped-entry", Provider: "openai", Model: "gpt-4o",
			AvoidedCOGSUSD: 2.0, AnswerSHA256: SHA256Hex([]byte("a")), PromptSHA256: SHA256Hex([]byte("p")),
		}
		res, err := m.MintServedHit(ctx, h)
		if err != nil || !res.Minted {
			t.Fatalf("seed %d: res=%+v err=%v", i, res, err)
		}
		ids = append(ids, res.RequestID)
	}
	// revoke 3 of them via the orchestrator
	r := NewRevoker(pool, ledger)
	rep, _ := r.RevokeHeldMints(ctx, ids[:3])
	if rep.Totals[OutcomeRevoked] != 3 {
		t.Fatalf("expected 3 revoked, got %v", rep.Totals)
	}
	// a further mint of the entry must STILL be capped (revoked rows remain, consuming budget)
	res, err := m.MintServedHit(ctx, ServedHit{
		RequestID: "cap-rk-after", RequesterWorkspace: "wsB-after", ContributorWorkspace: "wsA",
		Layer: "exact", EntryID: "capped-entry", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0, AnswerSHA256: SHA256Hex([]byte("a")), PromptSHA256: SHA256Hex([]byte("p")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Capped || res.CapReason != "per_entry" {
		t.Fatalf("res=%+v — revoked mints MUST still count toward the cap (no budget refund on revoke)", res)
	}
}

// mustScan runs a parameterless SELECT and scans into dest (test helper).
func mustScan(t *testing.T, pool *pgxpool.Pool, sql string, dest ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql).Scan(dest...); err != nil {
		t.Fatalf("scan %q: %v", sql, err)
	}
}
