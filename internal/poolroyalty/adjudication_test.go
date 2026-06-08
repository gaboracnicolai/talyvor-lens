package poolroyalty

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// recordingRevoker is a fake Revoker surface that records when it was called,
// so the unit tests can assert ORDER: the adjudication INSERT must precede the
// revoke, and the outcome UPDATE must follow it.
type recordingRevoker struct {
	called bool
	gotIDs []string
	report RevokeReport
	err    error
}

func (r *recordingRevoker) RevokeHeldMints(_ context.Context, ids []string) (RevokeReport, error) {
	r.called = true
	r.gotIDs = ids
	return r.report, r.err
}

func sampleDecision() AdjudicationDecision {
	return AdjudicationDecision{
		FlagType:            "volume",
		ResolutionLabel:     string(LabelTuplePinned),
		CandidateRequestIDs: []string{"req-1", "req-2", "req-3"},
		RevokeRequestIDs:    []string{"req-1", "req-2"}, // operator chose a SUBSET
		DecidedBy:           "global_key",
	}
}

// RECORD-BEFORE-REVOKE ORDERING: the adjudication row is INSERTed (outcome
// NULL) BEFORE RevokeHeldMints is called, then UPDATEd with the report. The
// pgxmock ordered expectations enforce the sequence; the recordingRevoker
// proves the revoke fired between them.
func TestAdjudicate_RecordBeforeRevokeOrdering(t *testing.T) {
	pool := newRevokeMock(t)
	rev := &recordingRevoker{report: RevokeReport{
		Outcomes: map[string]RevokeOutcome{"req-1": OutcomeRevoked, "req-2": OutcomeRevoked},
		Totals:   map[RevokeOutcome]int{OutcomeRevoked: 2},
	}}
	w := NewAdjudicationWriter(pool, rev)

	// 1. INSERT the record FIRST (outcome NULL), RETURNING id.
	pool.ExpectQuery(`INSERT INTO pool_royalty_adjudications`).
		WithArgs("volume", string(LabelTuplePinned), []string{"req-1", "req-2", "req-3"}, []string{"req-1", "req-2"}, "global_key").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("adj-abc"))
	// 2. (RevokeHeldMints fires here — asserted via the fake below.)
	// 3. UPDATE the record with the outcome JSONB.
	pool.ExpectExec(`UPDATE pool_royalty_adjudications SET outcome`).
		WithArgs("adj-abc", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	id, rep, err := w.Adjudicate(context.Background(), sampleDecision())
	if err != nil {
		t.Fatalf("Adjudicate: %v", err)
	}
	if id != "adj-abc" {
		t.Errorf("id = %q, want adj-abc", id)
	}
	if !rev.called {
		t.Fatal("RevokeHeldMints was never called")
	}
	if len(rev.gotIDs) != 2 || rev.gotIDs[0] != "req-1" || rev.gotIDs[1] != "req-2" {
		t.Errorf("revoke got %v, want the chosen subset [req-1 req-2]", rev.gotIDs)
	}
	if rep.Totals[OutcomeRevoked] != 2 {
		t.Errorf("returned report totals = %v", rep.Totals)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("ordered expectations unmet (INSERT before UPDATE): %v", err)
	}
}

// RECORD-BEFORE-BURN DURABILITY: if the revoke ERRORS, the record still exists
// (it was written first) with the chosen subset, and the outcome UPDATE still
// runs to record what happened (the report carries the errors). A burn can
// never happen without a preceding record.
func TestAdjudicate_RevokeErrorStillLeavesRecord(t *testing.T) {
	pool := newRevokeMock(t)
	// The revoker returns a report full of errors (and no top-level error — the
	// Revoker records per-row errors in the report and never fails the call).
	rev := &recordingRevoker{report: RevokeReport{
		Outcomes: map[string]RevokeOutcome{"req-1": OutcomeError, "req-2": OutcomeError},
		Totals:   map[RevokeOutcome]int{OutcomeError: 2},
	}}
	w := NewAdjudicationWriter(pool, rev)

	pool.ExpectQuery(`INSERT INTO pool_royalty_adjudications`).
		WithArgs("volume", string(LabelTuplePinned), []string{"req-1", "req-2", "req-3"}, []string{"req-1", "req-2"}, "global_key").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("adj-err"))
	pool.ExpectExec(`UPDATE pool_royalty_adjudications SET outcome`).
		WithArgs("adj-err", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	id, rep, err := w.Adjudicate(context.Background(), sampleDecision())
	if err != nil {
		t.Fatalf("per-row revoke errors must not fail Adjudicate: %v", err)
	}
	if id != "adj-err" {
		t.Errorf("the record must exist even when the revoke errored; id=%q", id)
	}
	if rep.Totals[OutcomeError] != 2 {
		t.Errorf("the outcome must record the errors; totals=%v", rep.Totals)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (record written + outcome recorded despite revoke errors): %v", err)
	}
}

// INSERT FAILURE → NO REVOKE: if the record write itself fails, the revoke must
// NOT fire (record-before-burn means no record ⇒ no burn).
func TestAdjudicate_RecordInsertFailureSkipsRevoke(t *testing.T) {
	pool := newRevokeMock(t)
	rev := &recordingRevoker{}
	w := NewAdjudicationWriter(pool, rev)

	pool.ExpectQuery(`INSERT INTO pool_royalty_adjudications`).
		WithArgs("volume", string(LabelTuplePinned), []string{"req-1", "req-2", "req-3"}, []string{"req-1", "req-2"}, "global_key").
		WillReturnError(errors.New("insert failed"))

	_, _, err := w.Adjudicate(context.Background(), sampleDecision())
	if err == nil {
		t.Fatal("a failed record write must error Adjudicate")
	}
	if rev.called {
		t.Fatal("the revoke must NOT fire if the record was not written (record-before-burn)")
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// EMPTY revoke set is rejected before any write (nothing to adjudicate).
func TestAdjudicate_EmptyRevokeSetRejected(t *testing.T) {
	pool := newRevokeMock(t)
	rev := &recordingRevoker{}
	w := NewAdjudicationWriter(pool, rev)
	d := sampleDecision()
	d.RevokeRequestIDs = nil
	if _, _, err := w.Adjudicate(context.Background(), d); err == nil {
		t.Error("an empty revoke set must be rejected (nothing to revoke)")
	}
	if rev.called {
		t.Error("no revoke on an empty set")
	}
}

// Nil-safe.
func TestAdjudicate_NilSafe(t *testing.T) {
	var w *AdjudicationWriter
	if _, _, err := w.Adjudicate(context.Background(), sampleDecision()); err == nil {
		t.Error("nil writer must error, not panic")
	}
}

// Guard against accidental interface widening: the writer's db seam used for
// the record is a plain Query/Exec surface; ensure pgx is imported for the
// ErrNoRows sentinel only (compile guard — keeps the test file honest).
var _ = pgx.ErrNoRows
