package poolroyalty

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase-3 Item 3 (the clearer half) — the settlement clearer promotes
// EXAMINED-clean-and-DUE held rows to 'cleared' so the fail-closed sweeper can
// settle them; it holds everything else. The four cases below are the whole
// safety story:
//   - examined + clean + due       → CLEARED (adjudicated, may settle)
//   - examined + FLAGGED           → held    (a ring never becomes cleared)
//   - NOT examined (detector blind) → held    (fail-closed: unadjudicated never settles)
//   - examined + clean but NOT due → held    (the detector keeps the full window)

type fakePartition struct {
	examined []string
	flags    []RingFlag
	err      error
}

func (f fakePartition) DetectAndPartition(_ context.Context, _ time.Duration) ([]string, []RingFlag, error) {
	return f.examined, f.flags, f.err
}

// insertHeldClaim writes a held pool_royalty_mints row with finalize_after = now()
// - dueAgo (positive dueAgo ⇒ already DUE; negative ⇒ not yet due).
func insertHeldClaim(t *testing.T, pool *pgxpool.Pool, reqID, contributor string, dueAgo time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO pool_royalty_mints (request_id, requester_workspace_id, contributor_workspace_id, layer, minted_amount, status, finalize_after)
		 VALUES ($1, 'wsReq', $2, 'exact', 1000, 'held', now() - $3::bigint * interval '1 microsecond')`,
		reqID, contributor, dueAgo.Microseconds())
	if err != nil {
		t.Fatalf("insert held claim %s: %v", reqID, err)
	}
}

func TestSettlementClearer_ClearsExaminedCleanDue_HoldsRest_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()

	insertHeldClaim(t, pool, "a", "wsA", time.Hour)   // examined, clean, DUE
	insertHeldClaim(t, pool, "b", "wsB", time.Hour)   // examined, FLAGGED, due
	insertHeldClaim(t, pool, "c", "wsC", time.Hour)   // NOT examined, due
	insertHeldClaim(t, pool, "d", "wsD", -time.Hour)  // examined, clean, NOT due (future finalize_after)

	det := fakePartition{examined: []string{"a", "b", "d"}, flags: []RingFlag{{RequestID: "b"}}}
	clearer := NewSettlementClearer(det, pool, "pool_royalty_mints", func() bool { return true }, time.Hour)

	n, err := clearer.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared %d, want 1 (only the examined+clean+due row 'a')", n)
	}
	for id, want := range map[string]string{"a": "cleared", "b": "held", "c": "held", "d": "held"} {
		if got := mintStatusOf(t, pool, id); got != want {
			t.Fatalf("row %q status=%q, want %q", id, got, want)
		}
	}
}

// Fail-closed: a detector error clears NOTHING — an examined-clean-due row stays
// held (never settles) rather than being cleared on an unknown picture.
func TestSettlementClearer_DetectorError_ClearsNothing_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()
	insertHeldClaim(t, pool, "e", "wsE", time.Hour)

	det := fakePartition{err: context.DeadlineExceeded}
	clearer := NewSettlementClearer(det, pool, "pool_royalty_mints", func() bool { return true }, time.Hour)
	n, err := clearer.RunOnce(ctx)
	if err == nil {
		t.Fatal("a detector error must propagate (fail-closed), got nil")
	}
	if n != 0 {
		t.Fatalf("cleared %d on a detector error, want 0 (fail-closed)", n)
	}
	if st := mintStatusOf(t, pool, "e"); st != "held" {
		t.Fatalf("status=%q, want held (nothing cleared on an unknown picture)", st)
	}
}

// Default OFF: a nil/false enable gate makes RunOnce a total no-op.
func TestSettlementClearer_DefaultOff_Inert_Integration(t *testing.T) {
	pool := revokerTestPool(t)
	ctx := context.Background()
	insertHeldClaim(t, pool, "f", "wsF", time.Hour)
	clearer := NewSettlementClearer(fakePartition{examined: []string{"f"}}, pool, "pool_royalty_mints", func() bool { return false }, time.Hour)
	if n, err := clearer.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("disabled clearer must be inert: n=%d err=%v", n, err)
	}
	if st := mintStatusOf(t, pool, "f"); st != "held" {
		t.Fatalf("status=%q, want held (disabled clearer changes nothing)", st)
	}
}
