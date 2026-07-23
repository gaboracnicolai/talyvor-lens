package poolroyalty

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PR3 — distill revoke / adjudication (the held-mint claw-back). Proofs on real PG,
// asserted on the LEDGER (held balance burned) + the claim/audit tables. The Revoker
// + AdjudicationWriter are parameterized by table; RevokeHeldTx is already generic.

// distillAdjudicationsDDL mirrors migration 0063 (the harness uses inline DDL, not
// the migration chain).
const distillAdjudicationsDDL = `CREATE TABLE IF NOT EXISTS distill_royalty_adjudications (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    flag_type             TEXT NOT NULL,
    resolution_label      TEXT NOT NULL,
    candidate_request_ids TEXT[] NOT NULL,
    revoked_request_ids   TEXT[] NOT NULL,
    decided_by            TEXT NOT NULL,
    outcome               JSONB,
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now()
)`

func createDistillAdjudications(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), distillAdjudicationsDDL); err != nil {
		t.Fatalf("create distill_royalty_adjudications: %v", err)
	}
}

func mintStatus(t *testing.T, pool *pgxpool.Pool, requestID string) string {
	t.Helper()
	var st string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM distill_royalty_mints WHERE request_id=$1`, requestID).Scan(&st); err != nil {
		t.Fatalf("mint status: %v", err)
	}
	return st
}

// (PR3.a) A flagged distill mint is clawed back EXACTLY ONCE (CAS, no double-revoke);
// the held balance is burned; a second revoke is an idempotent no-op (no double-burn).
func TestDistillRevoke_ClawsBackExactlyOnce_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "h1", 4.0)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	if n, err := m.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("seed mint: minted %d err=%v, want 1", n, err)
	}
	rid := SHA256Hex([]byte("wsA:wsB:h1"))
	if _, held := balances(t, pool, "wsA"); held != micro(20) {
		t.Fatalf("pre-revoke held=%v, want 20 LENS (0.5 × $4 × 10 LENS/$ peg)", held)
	}

	rev := NewRevokerForTable(pool, ledger, "distill_royalty_mints")
	rep, _ := rev.RevokeHeldMints(ctx, []string{rid})
	if rep.Outcomes[rid] != OutcomeRevoked {
		t.Fatalf("first revoke: outcome %v, want revoked", rep.Outcomes[rid])
	}
	if st := mintStatus(t, pool, rid); st != "revoked" {
		t.Fatalf("status=%v, want revoked", st)
	}
	if _, held := balances(t, pool, "wsA"); held != 0.0 {
		t.Fatalf("post-revoke held=%v, want 0.0 (clawed back)", held)
	}

	// Exactly-once: a SECOND revoke is a no-op — skipped_already_revoked, NO double-burn.
	rep2, _ := rev.RevokeHeldMints(ctx, []string{rid})
	if rep2.Outcomes[rid] != OutcomeSkippedAlreadyRevoked {
		t.Fatalf("second revoke: outcome %v, want skipped_already_revoked", rep2.Outcomes[rid])
	}
	if _, held := balances(t, pool, "wsA"); held != 0.0 {
		t.Fatalf("double-revoke must not double-burn; held=%v, want 0.0", held)
	}

	// And the revoked mint never finalizes into supply: the finalize sweeper's CAS
	// matches only status='held', so a revoked row is skipped → no counted ledger row.
	sw := NewFinalizeSweeper(pool, ledger, "distill_royalty_mints")
	if _, err := sw.RunOnce(ctx); err != nil {
		t.Fatalf("finalize sweep: %v", err)
	}
	if st := mintStatus(t, pool, rid); st != "revoked" {
		t.Fatalf("revoked mint must stay revoked after a finalize sweep; got %v", st)
	}
	if bal, _ := balances(t, pool, "wsA"); bal != 0.0 {
		t.Fatalf("revoked mint must not finalize into spendable/supply; bal=%v, want 0.0", bal)
	}
}

// (PR3.b) Adjudicate writes the audit record FIRST, then revokes EXACTLY the chosen
// subset; an un-chosen mint is untouched (record-before-revoke + honored subset).
func TestDistillAdjudicate_RecordBeforeRevoke_SubsetHonored_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	createDistillAdjudications(t, pool)
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "h1", 4.0) // will be revoked
	seedBasis(t, pool, "wsA", "wsC", "h2", 4.0) // will be KEPT (untouched)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	if n, _ := m.RunOnce(ctx); n != 2 {
		t.Fatalf("seed: want 2 held mints")
	}
	ridRevoke := SHA256Hex([]byte("wsA:wsB:h1"))
	ridKeep := SHA256Hex([]byte("wsA:wsC:h2"))

	rev := NewRevokerForTable(pool, ledger, "distill_royalty_mints")
	adj := NewAdjudicationWriterForTable(pool, rev, "distill_royalty_adjudications")
	id, rep, err := adj.Adjudicate(ctx, AdjudicationDecision{
		FlagType:            "self_dealing",
		ResolutionLabel:     "pair_coarse",
		CandidateRequestIDs: []string{ridRevoke, ridKeep},
		RevokeRequestIDs:    []string{ridRevoke}, // operator chose ONLY h1
		DecidedBy:           "admin1",
	})
	if err != nil {
		t.Fatalf("adjudicate: %v", err)
	}
	if id == "" {
		t.Fatal("adjudicate must return a record id")
	}
	if rep.Outcomes[ridRevoke] != OutcomeRevoked {
		t.Fatalf("h1: outcome %v, want revoked", rep.Outcomes[ridRevoke])
	}

	// The audit row exists, with the outcome recorded (record-before-revoke completed).
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM distill_royalty_adjudications WHERE id=$1 AND outcome IS NOT NULL`, id).Scan(&cnt); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("expected 1 completed adjudication row, got %d", cnt)
	}
	// h1 revoked, h2 untouched (still held — the subset was honored exactly).
	if st := mintStatus(t, pool, ridRevoke); st != "revoked" {
		t.Fatalf("h1 status=%v, want revoked", st)
	}
	if st := mintStatus(t, pool, ridKeep); st != "held" {
		t.Fatalf("h2 status=%v, want held (NOT in the revoke subset)", st)
	}
}

// (PR3.c) A FINALIZED distill mint is NOT revocable: the CAS matches only status='held',
// so a final row → skipped_not_held, never burned.
func TestDistillRevoke_FinalNotRevocable_Integration(t *testing.T) {
	pool, ledger := distillMintHarness(t)
	ctx := context.Background()
	verifyWorkspace(t, pool, "wsA")
	seedBasis(t, pool, "wsA", "wsB", "h1", 4.0)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Nanosecond) // finalize-eligible immediately
	if n, err := m.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("seed mint: minted %d err=%v, want 1", n, err)
	}
	rid := SHA256Hex([]byte("wsA:wsB:h1"))

	sw := NewFinalizeSweeper(pool, ledger, "distill_royalty_mints")
	if _, err := sw.RunOnce(ctx); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if st := mintStatus(t, pool, rid); st != "final" {
		t.Fatalf("pre-revoke status=%v, want final", st)
	}
	balBefore, _ := balances(t, pool, "wsA")

	rev := NewRevokerForTable(pool, ledger, "distill_royalty_mints")
	rep, _ := rev.RevokeHeldMints(ctx, []string{rid})
	if rep.Outcomes[rid] != OutcomeSkippedNotHeld {
		t.Fatalf("final mint: outcome %v, want skipped_not_held", rep.Outcomes[rid])
	}
	if st := mintStatus(t, pool, rid); st != "final" {
		t.Fatalf("final mint must stay final; got %v", st)
	}
	if balAfter, _ := balances(t, pool, "wsA"); balAfter != balBefore {
		t.Fatalf("final mint must NOT be burned; bal %v→%v", balBefore, balAfter)
	}
}
