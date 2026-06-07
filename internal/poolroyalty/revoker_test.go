package poolroyalty

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// fakeRevokeLedger records RevokeHeldTx calls (the real *mining.LedgerStore
// satisfies the same signature). It lets the unit tests assert the held-burn
// fires exactly when — and only when — the CAS transitioned a row.
type revokeCall struct {
	workspaceID string
	amount      float64
}

type fakeRevokeLedger struct {
	calls []revokeCall
	err   error
}

func (f *fakeRevokeLedger) RevokeHeldTx(_ context.Context, _ pgx.Tx, ws string, amount float64, _ string, _ map[string]interface{}) error {
	f.calls = append(f.calls, revokeCall{workspaceID: ws, amount: amount})
	return f.err
}

func newRevokeMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// SINGLE HELD → REVOKED: the CAS RETURNS the contributor + amount (so it was
// held), the held-burn fires in the SAME tx, commit. Outcome revoked.
func TestRevokeHeldMints_SingleRevoked(t *testing.T) {
	pool := newRevokeMock(t)
	led := &fakeRevokeLedger{}
	r := NewRevoker(pool, led)

	pool.ExpectBegin()
	pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).
		WithArgs("req-1").
		WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"}).AddRow("wsA", 1.5))
	pool.ExpectCommit()

	rep, err := r.RevokeHeldMints(context.Background(), []string{"req-1"})
	if err != nil {
		t.Fatalf("RevokeHeldMints: %v", err)
	}
	if rep.Outcomes["req-1"] != OutcomeRevoked {
		t.Errorf("outcome = %q, want revoked", rep.Outcomes["req-1"])
	}
	if rep.Totals[OutcomeRevoked] != 1 {
		t.Errorf("totals[revoked] = %d, want 1", rep.Totals[OutcomeRevoked])
	}
	if len(led.calls) != 1 || led.calls[0].workspaceID != "wsA" || led.calls[0].amount != 1.5 {
		t.Errorf("RevokeHeldTx calls = %+v, want one wsA/1.5", led.calls)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// FINALITY GUARD: a status='final' mint → CAS affects 0 rows (no RETURNING) →
// the classify SELECT reads 'final' → skipped_not_held, NO burn. A finalized
// mint is NEVER revocable. The deferred Rollback ends the (write-free) tx.
func TestRevokeHeldMints_FinalizedSkippedNoBurn(t *testing.T) {
	pool := newRevokeMock(t)
	led := &fakeRevokeLedger{}
	r := NewRevoker(pool, led)

	pool.ExpectBegin()
	pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).
		WithArgs("req-final").
		WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"})) // 0 rows
	pool.ExpectQuery(`SELECT status FROM pool_royalty_mints`).
		WithArgs("req-final").
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("final"))
	pool.ExpectRollback()

	rep, err := r.RevokeHeldMints(context.Background(), []string{"req-final"})
	if err != nil {
		t.Fatalf("RevokeHeldMints: %v", err)
	}
	if rep.Outcomes["req-final"] != OutcomeSkippedNotHeld {
		t.Errorf("outcome = %q, want skipped_not_held", rep.Outcomes["req-final"])
	}
	if len(led.calls) != 0 {
		t.Fatalf("a finalized mint must NEVER burn; calls=%d", len(led.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// ALREADY REVOKED: CAS 0 rows, classify reads 'revoked' → skipped_already_revoked,
// no second burn (idempotency at the per-row level).
func TestRevokeHeldMints_AlreadyRevokedSkipped(t *testing.T) {
	pool := newRevokeMock(t)
	led := &fakeRevokeLedger{}
	r := NewRevoker(pool, led)

	pool.ExpectBegin()
	pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).
		WithArgs("req-r").
		WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"}))
	pool.ExpectQuery(`SELECT status FROM pool_royalty_mints`).
		WithArgs("req-r").
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("revoked"))
	pool.ExpectRollback()

	rep, _ := r.RevokeHeldMints(context.Background(), []string{"req-r"})
	if rep.Outcomes["req-r"] != OutcomeSkippedAlreadyRevoked {
		t.Errorf("outcome = %q, want skipped_already_revoked", rep.Outcomes["req-r"])
	}
	if len(led.calls) != 0 {
		t.Error("already-revoked must not burn again")
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// NOT FOUND: a request_id with no row → CAS 0 rows, classify SELECT returns
// ErrNoRows → skipped_not_found, NO error, NO burn, NO panic on empty result.
func TestRevokeHeldMints_NotFound(t *testing.T) {
	pool := newRevokeMock(t)
	led := &fakeRevokeLedger{}
	r := NewRevoker(pool, led)

	pool.ExpectBegin()
	pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).
		WithArgs("ghost").
		WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"}))
	pool.ExpectQuery(`SELECT status FROM pool_royalty_mints`).
		WithArgs("ghost").
		WillReturnError(pgx.ErrNoRows)
	pool.ExpectRollback()

	rep, err := r.RevokeHeldMints(context.Background(), []string{"ghost"})
	if err != nil {
		t.Fatalf("not-found must not error the whole call: %v", err)
	}
	if rep.Outcomes["ghost"] != OutcomeSkippedNotFound {
		t.Errorf("outcome = %q, want skipped_not_found", rep.Outcomes["ghost"])
	}
	if len(led.calls) != 0 {
		t.Error("not-found must not burn")
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// ALL-GARBAGE SET: every request_id never existed → each skipped_not_found,
// zero errors, zero burns, totals correct.
func TestRevokeHeldMints_AllGarbage(t *testing.T) {
	pool := newRevokeMock(t)
	led := &fakeRevokeLedger{}
	r := NewRevoker(pool, led)

	ghosts := []string{"g1", "g2", "g3"}
	for _, g := range ghosts {
		pool.ExpectBegin()
		pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).
			WithArgs(g).
			WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"}))
		pool.ExpectQuery(`SELECT status FROM pool_royalty_mints`).
			WithArgs(g).
			WillReturnError(pgx.ErrNoRows)
		pool.ExpectRollback()
	}

	rep, err := r.RevokeHeldMints(context.Background(), ghosts)
	if err != nil {
		t.Fatalf("all-garbage must not error: %v", err)
	}
	if rep.Totals[OutcomeSkippedNotFound] != 3 || len(led.calls) != 0 {
		t.Errorf("want 3 not_found / 0 burns; totals=%v calls=%d", rep.Totals, len(led.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// MIXED SET: [held, final, already-revoked, nonexistent] → each its own
// per-row outcome; the burn fires only for the held row; totals correct.
func TestRevokeHeldMints_MixedSet(t *testing.T) {
	pool := newRevokeMock(t)
	led := &fakeRevokeLedger{}
	r := NewRevoker(pool, led)

	// held → revoked
	pool.ExpectBegin()
	pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).WithArgs("held-1").
		WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"}).AddRow("wsA", 2.0))
	pool.ExpectCommit()
	// final → skipped_not_held
	pool.ExpectBegin()
	pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).WithArgs("final-1").
		WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"}))
	pool.ExpectQuery(`SELECT status FROM pool_royalty_mints`).WithArgs("final-1").
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("final"))
	pool.ExpectRollback()
	// revoked → skipped_already_revoked
	pool.ExpectBegin()
	pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).WithArgs("rev-1").
		WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"}))
	pool.ExpectQuery(`SELECT status FROM pool_royalty_mints`).WithArgs("rev-1").
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("revoked"))
	pool.ExpectRollback()
	// nonexistent → skipped_not_found
	pool.ExpectBegin()
	pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).WithArgs("ghost-1").
		WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"}))
	pool.ExpectQuery(`SELECT status FROM pool_royalty_mints`).WithArgs("ghost-1").
		WillReturnError(pgx.ErrNoRows)
	pool.ExpectRollback()

	rep, err := r.RevokeHeldMints(context.Background(), []string{"held-1", "final-1", "rev-1", "ghost-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]RevokeOutcome{
		"held-1": OutcomeRevoked, "final-1": OutcomeSkippedNotHeld,
		"rev-1": OutcomeSkippedAlreadyRevoked, "ghost-1": OutcomeSkippedNotFound,
	}
	for id, w := range want {
		if rep.Outcomes[id] != w {
			t.Errorf("%s outcome = %q, want %q", id, rep.Outcomes[id], w)
		}
	}
	if rep.Totals[OutcomeRevoked] != 1 || len(led.calls) != 1 || led.calls[0].workspaceID != "wsA" {
		t.Errorf("only the held row burns; totals=%v calls=%+v", rep.Totals, led.calls)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// BURN FAILURE rolls back the CAS too: if RevokeHeldTx errors after a
// successful CAS, the whole row tx rolls back (no orphan status flip, no burn),
// outcome=error. Atomicity of the flip+burn pair.
func TestRevokeHeldMints_BurnFailureRollsBackFlip(t *testing.T) {
	pool := newRevokeMock(t)
	led := &fakeRevokeLedger{err: errors.New("held ledger down")}
	r := NewRevoker(pool, led)

	pool.ExpectBegin()
	pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).WithArgs("req-x").
		WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"}).AddRow("wsA", 1.0))
	pool.ExpectRollback() // burn failed → flip discarded with it

	rep, err := r.RevokeHeldMints(context.Background(), []string{"req-x"})
	if err != nil {
		t.Fatalf("a per-row error must not fail the whole call: %v", err)
	}
	if rep.Outcomes["req-x"] != OutcomeError {
		t.Errorf("outcome = %q, want error", rep.Outcomes["req-x"])
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (CAS flip must roll back with the failed burn): %v", err)
	}
}

// Nil/empty safety.
func TestRevokeHeldMints_NilAndEmpty(t *testing.T) {
	var nilR *Revoker
	if rep, err := nilR.RevokeHeldMints(context.Background(), []string{"x"}); err != nil || len(rep.Outcomes) != 0 {
		t.Errorf("nil revoker must no-op; rep=%+v err=%v", rep, err)
	}
	pool := newRevokeMock(t)
	r := NewRevoker(pool, &fakeRevokeLedger{})
	if rep, err := r.RevokeHeldMints(context.Background(), nil); err != nil || len(rep.Outcomes) != 0 {
		t.Errorf("empty set must no-op; rep=%+v err=%v", rep, err)
	}
}

// DEDUP: a request_id repeated within one batch is processed ONCE — the report
// stays internally consistent (sum(Totals) == len(Outcomes)) and the held-burn
// fires once. (The per-row CAS already makes the burn idempotent; this guards
// the AUDIT report, the product's accountability surface.)
func TestRevokeHeldMints_DuplicateInBatchProcessedOnce(t *testing.T) {
	pool := newRevokeMock(t)
	led := &fakeRevokeLedger{}
	r := NewRevoker(pool, led)

	// Only ONE tx expected despite the id appearing 3×.
	pool.ExpectBegin()
	pool.ExpectQuery(`UPDATE pool_royalty_mints SET status = 'revoked'`).
		WithArgs("dup").
		WillReturnRows(pgxmock.NewRows([]string{"contributor_workspace_id", "minted_amount"}).AddRow("wsA", 1.0))
	pool.ExpectCommit()

	rep, err := r.RevokeHeldMints(context.Background(), []string{"dup", "dup", "dup"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Outcomes) != 1 || rep.Outcomes["dup"] != OutcomeRevoked {
		t.Errorf("outcomes = %+v, want one revoked", rep.Outcomes)
	}
	sumTotals := 0
	for _, n := range rep.Totals {
		sumTotals += n
	}
	if sumTotals != len(rep.Outcomes) {
		t.Errorf("report inconsistent: sum(Totals)=%d != len(Outcomes)=%d", sumTotals, len(rep.Outcomes))
	}
	if len(led.calls) != 1 {
		t.Errorf("held burned %d times for a duplicated id, want 1", len(led.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (only ONE tx for a duplicated id): %v", err)
	}
}
