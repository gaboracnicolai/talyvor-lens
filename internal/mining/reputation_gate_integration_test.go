package mining

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func mustCount(t *testing.T, pool *pgxpool.Pool, sql string, dest *int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql).Scan(dest); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
}

// PR2 — the reputation gate (GetPendingTask floor) + admin re-entry (Reset). The property that
// matters most: the gate NEVER benches an innocent (new / dormant) annotator. Reuses repHarness.

// (1) GATE — does NOT bench the innocent; benches only a sub-floor (actively-disagreeing) one.
func TestReputationGate_DoesNotBenchInnocent_Integration(t *testing.T) {
	pool, ledger := repHarness(t)
	ctx := context.Background()
	miner := NewAnnotationMiner(ledger, pool)
	store := NewReputationStore(pool)
	seedTask(t, pool, "src", time.Now().Add(time.Hour)) // one pending task (read-only; not claimed)

	// NEW annotator: no events → baseline 0.5 (>= 0.35) → MUST get a task.
	if task, err := miner.GetPendingTask(ctx, "newbie"); err != nil || task == nil {
		t.Errorf("NEW annotator (baseline) must get a task — never benched (task=%v err=%v)", task, err)
	}

	// DORMANT-DECAYED to baseline: earned above, then decay floored back at baseline → 0.5 → task.
	mustRecord(t, store, "dormant", "agreement_outcome", "g1", 0.2) // 0.7
	mustRecord(t, store, "dormant", "decay", "d1", -0.2)            // back to baseline 0.5
	if s := scoreOf(t, pool, "dormant"); math.Abs(s-ReputationBaseline) > 1e-9 {
		t.Fatalf("setup: dormant score %v want baseline", s)
	}
	if task, _ := miner.GetPendingTask(ctx, "dormant"); task == nil {
		t.Error("dormant-decayed annotator AT baseline must get a task (decay floors at baseline, never below)")
	}

	// SUB-FLOOR (active disagreement): score < 0.35 → NO task.
	mustRecord(t, store, "bad", "agreement_outcome", "tbad", -0.2) // 0.5 − 0.2 = 0.3 < 0.35
	if s := scoreOf(t, pool, "bad"); s >= AccessFloor {
		t.Fatalf("setup: bad score %v not sub-floor", s)
	}
	if task, _ := miner.GetPendingTask(ctx, "bad"); task != nil {
		t.Error("sub-floor annotator must be GATED (no task)")
	}

	// BOUNDARY: score EXACTLY at the floor (0.35) → gets a task (>= is inclusive).
	mustRecord(t, store, "edge", "agreement_outcome", "tedge", -0.15) // 0.5 − 0.15 = 0.35
	if s := scoreOf(t, pool, "edge"); math.Abs(s-AccessFloor) > 1e-9 {
		t.Fatalf("setup: edge score %v want exactly 0.35", s)
	}
	if task, _ := miner.GetPendingTask(ctx, "edge"); task == nil {
		t.Error("score exactly at the floor (0.35) must get a task (>= inclusive)")
	}
}

// (2) ADMIN RESET — restores to baseline by APPENDING an admin_reset event (prior events kept);
// the annotator is un-benched; a second reset is a no-op-ish (still lands at baseline).
func TestReputationReset_RestoresAndAppends_Integration(t *testing.T) {
	pool, ledger := repHarness(t)
	ctx := context.Background()
	miner := NewAnnotationMiner(ledger, pool)
	store := NewReputationStore(pool)
	seedTask(t, pool, "src", time.Now().Add(time.Hour))

	mustRecord(t, store, "bad", "agreement_outcome", "tbad", -0.3) // 0.2 < floor → benched
	if task, _ := miner.GetPendingTask(ctx, "bad"); task != nil {
		t.Fatal("setup: sub-floor annotator should be gated")
	}
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reputation_events WHERE annotator_id='bad'`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	newScore, err := store.Reset(ctx, "bad", "admin1", "reviewed appeal")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if math.Abs(newScore-ReputationBaseline) > 1e-9 {
		t.Errorf("post-reset score %v want baseline %v", newScore, ReputationBaseline)
	}
	// un-benched.
	if task, _ := miner.GetPendingTask(ctx, "bad"); task == nil {
		t.Error("post-reset annotator must get a task again")
	}
	// APPENDED, not mutated: one more event, prior events preserved, one admin_reset.
	var after, resets, outcomes int
	mustCount(t, pool, `SELECT count(*) FROM reputation_events WHERE annotator_id='bad'`, &after)
	mustCount(t, pool, `SELECT count(*) FROM reputation_events WHERE annotator_id='bad' AND kind='admin_reset'`, &resets)
	mustCount(t, pool, `SELECT count(*) FROM reputation_events WHERE annotator_id='bad' AND kind='agreement_outcome'`, &outcomes)
	if after != before+1 {
		t.Errorf("reset must APPEND (events %d→%d, want +1)", before, after)
	}
	if resets != 1 {
		t.Errorf("expected 1 admin_reset event, got %d", resets)
	}
	if outcomes != 1 {
		t.Errorf("prior agreement_outcome event must be PRESERVED (got %d, want 1)", outcomes)
	}

	// Idempotency shape: a second reset still lands at baseline (delta recomputed to 0).
	if s2, err := store.Reset(ctx, "bad", "admin1", "again"); err != nil || math.Abs(s2-ReputationBaseline) > 1e-9 {
		t.Errorf("second reset score %v err=%v want baseline (no-op-ish)", s2, err)
	}
}
