package mining

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PR3 — dormancy decay. The property that matters most: decay FLOORS AT BASELINE (never benches a
// dormant annotator). Reuses repHarness + seed helpers from the PR1/PR2 test files.

// seedAnnotationAt gives annotatorID an annotation `daysAgo` days old (sets last-activity for the
// dormancy check) on a fresh task.
func seedAnnotationAt(t *testing.T, pool *pgxpool.Pool, annotatorID string, daysAgo int) {
	t.Helper()
	tid := seedTask(t, pool, "src-"+annotatorID, time.Now().Add(time.Hour))
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO annotations (task_id, annotator_id, decision, created_at)
		 VALUES ($1, $2, 'a_better', now() - make_interval(days => $3))`,
		tid, annotatorID, daysAgo); err != nil {
		t.Fatalf("seed annotation -%dd: %v", daysAgo, err)
	}
}

// seedAboveDormant: annotatorID gets reputation baseline+rawSum and a last annotation daysAgo old.
func seedAboveDormant(t *testing.T, pool *pgxpool.Pool, store *ReputationStore, annotatorID string, rawSum float64, daysAgo int) {
	t.Helper()
	if rawSum != 0 {
		mustRecord(t, store, annotatorID, "agreement_outcome", "seed-"+annotatorID, rawSum)
	}
	seedAnnotationAt(t, pool, annotatorID, daysAgo)
}

func decayCountFor(t *testing.T, pool *pgxpool.Pool, annotatorID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM reputation_events WHERE kind='decay' AND annotator_id=$1`, annotatorID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// (1) DECAY FLOORS AT BASELINE — above-baseline erodes to baseline and STOPS; at/below baseline is a no-op.
func TestReputationDecay_FloorsAtBaseline_Integration(t *testing.T) {
	pool, _ := repHarness(t)
	ctx := context.Background()
	store := NewReputationStore(pool)

	seedAboveDormant(t, pool, store, "high", 0.2, 100) // 0.7, dormant 100d → erodes to baseline
	mustRecord(t, store, "mid", "agreement_outcome", "m1", 0.2)
	mustRecord(t, store, "mid", "agreement_outcome", "m2", -0.2) // raw_sum 0 → score == baseline
	seedAnnotationAt(t, pool, "mid", 100)
	mustRecord(t, store, "low", "agreement_outcome", "l1", -0.2) // 0.3, below baseline
	seedAnnotationAt(t, pool, "low", 100)

	if _, err := store.DecayDormant(ctx); err != nil {
		t.Fatal(err)
	}
	// above-baseline → eroded to EXACTLY baseline, never below (clamped).
	if s := scoreOf(t, pool, "high"); s < ReputationBaseline-1e-9 || math.Abs(s-ReputationBaseline) > 1e-9 {
		t.Errorf("long-dormant above-baseline → score %v, want exactly baseline %v (full erosion, clamped at baseline)", s, ReputationBaseline)
	}
	// at baseline → no decay event.
	if c := decayCountFor(t, pool, "mid"); c != 0 {
		t.Errorf("at-baseline annotator decayed (%d events) — must be a no-op", c)
	}
	// below baseline → no decay (only disagreement put them there; decay must not deepen it).
	if c := decayCountFor(t, pool, "low"); c != 0 {
		t.Errorf("below-baseline annotator decayed (%d events) — decay must not deepen disagreement", c)
	}
	if s := scoreOf(t, pool, "low"); math.Abs(s-0.3) > 1e-9 {
		t.Errorf("below-baseline score moved to %v, want 0.3 unchanged", s)
	}
}

// (2) ACTIVE ANNOTATORS NEVER DECAY — recent activity (within DormancyDays) → no decay, any score.
func TestReputationDecay_ActiveNeverDecays_Integration(t *testing.T) {
	pool, _ := repHarness(t)
	ctx := context.Background()
	store := NewReputationStore(pool)
	seedAboveDormant(t, pool, store, "active", 0.2, 1) // 0.7, last activity 1d ago → ACTIVE
	if _, err := store.DecayDormant(ctx); err != nil {
		t.Fatal(err)
	}
	if c := decayCountFor(t, pool, "active"); c != 0 {
		t.Errorf("active annotator decayed (%d) — only annotators dormant > %dd decay", c, DormancyDays)
	}
	if s := scoreOf(t, pool, "active"); math.Abs(s-0.7) > 1e-9 {
		t.Errorf("active annotator score moved to %v, want 0.7 unchanged", s)
	}
}

// (3) RECOVERABLE + NEVER BENCHED — decayed-to-baseline still passes the PR2 gate; reputation re-climbs.
func TestReputationDecay_RecoverableAndNeverBenched_Integration(t *testing.T) {
	pool, ledger := repHarness(t)
	ctx := context.Background()
	miner := NewAnnotationMiner(ledger, pool)
	store := NewReputationStore(pool)
	seedTask(t, pool, "tasksrc", time.Now().Add(time.Hour)) // a claimable task

	seedAboveDormant(t, pool, store, "dorm", 0.2, 100) // 0.7, dormant → decays to baseline
	if _, err := store.DecayDormant(ctx); err != nil {
		t.Fatal(err)
	}
	if s := scoreOf(t, pool, "dorm"); math.Abs(s-ReputationBaseline) > 1e-9 {
		t.Fatalf("decayed to %v, want baseline", s)
	}
	// NEVER BENCHED: baseline 0.5 > AccessFloor 0.35 → still gets tasks (ties PR3 to PR2's guarantee).
	if task, _ := miner.GetPendingTask(ctx, "dorm"); task == nil {
		t.Error("dormant-decayed annotator (at baseline) must STILL get tasks — decay never benches the innocent")
	}
	// RECOVERABLE: a fresh agreement_outcome climbs back above baseline (decay is not a ratchet).
	mustRecord(t, store, "dorm", "agreement_outcome", "recover", 0.1)
	if s := scoreOf(t, pool, "dorm"); s <= ReputationBaseline {
		t.Errorf("post-recovery score %v, want > baseline (decay is reversible)", s)
	}
}

// (4) IDEMPOTENT PER DAY + CATCH-UP — N dormant days fold into ONE event; a re-run is a no-op.
func TestReputationDecay_IdempotentPerDay_Integration(t *testing.T) {
	pool, _ := repHarness(t)
	ctx := context.Background()
	store := NewReputationStore(pool)
	seedAboveDormant(t, pool, store, "x", 0.2, 10) // 0.7, dormant 10d → 3 days past the 7d threshold

	n1, err := store.DecayDormant(ctx)
	if err != nil || n1 != 1 {
		t.Fatalf("first sweep decayed %d (err %v), want 1", n1, err)
	}
	s1 := scoreOf(t, pool, "x")
	if math.Abs(s1-0.67) > 1e-9 {
		t.Errorf("10d-dormant (3 dormant days) → score %v, want 0.67 (3·0.01 catch-up in ONE event)", s1)
	}
	if c := decayCountFor(t, pool, "x"); c != 1 {
		t.Errorf("want 1 decay event (single catch-up), got %d", c)
	}
	// RE-RUN same day → no new event, score stable (idempotent per day via the UNIQUE).
	n2, _ := store.DecayDormant(ctx)
	if n2 != 0 {
		t.Errorf("re-run decayed %d, want 0 (idempotent per day)", n2)
	}
	if c := decayCountFor(t, pool, "x"); c != 1 {
		t.Errorf("re-run added a decay event (count %d, want 1) — must be idempotent per day", c)
	}
	if s2 := scoreOf(t, pool, "x"); math.Abs(s1-s2) > 1e-9 {
		t.Errorf("score changed on re-run %v→%v", s1, s2)
	}
}

// (5) MONEY-BOUNDARY — a DECAYED annotator earns byte-identical to a non-decayed one on held tasks.
func TestReputationDecay_MoneyBoundary_EarningInvariant_Integration(t *testing.T) {
	pool, ledger := repHarness(t)
	ctx := context.Background()
	miner := NewAnnotationMiner(ledger, pool)
	store := NewReputationStore(pool)

	seedAboveDormant(t, pool, store, "decayed", 0.2, 100) // 0.7, dormant → decays
	if _, err := store.DecayDormant(ctx); err != nil {
		t.Fatal(err)
	}
	mustRecord(t, store, "fresh", "agreement_outcome", "f1", 0.4) // 0.9, no annotations → never dormant
	for _, ws := range []string{"decayed", "fresh"} {
		if _, err := pool.Exec(ctx, `INSERT INTO annotator_stakes (workspace_id, staked) VALUES ($1, 20000000)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	submit := func(ws string) {
		tid := seedTask(t, pool, "crowdsrc", time.Now().Add(time.Hour))
		for i := 0; i < 3; i++ {
			seedAnnotation(t, pool, tid, fmt.Sprintf("c-%s-%d", tid, i), "a_better")
		}
		if err := miner.SubmitAnnotation(ctx, Annotation{TaskID: tid, AnnotatorID: ws, Decision: "a_better", Confidence: 3}); err != nil {
			t.Fatalf("submit %s: %v", ws, err)
		}
	}
	submit("decayed")
	submit("fresh")
	earn := func(ws string) int64 {
		var v int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0) FROM lens_token_ledger WHERE workspace_id=$1 AND type=$2`, ws, TypeAnnotationMine).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	ed, ef := earn("decayed"), earn("fresh")
	if ed != ef {
		t.Errorf("DECAYED vs fresh earn differently: %v vs %v — earning must be reputation-invariant", ed, ef)
	}
	if ed != micro(0.15) {
		t.Errorf("earning %v µLENS, want micro(0.15) (base + bonus, decay-independent)", ed)
	}
}
