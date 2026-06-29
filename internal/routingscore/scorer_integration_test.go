package routingscore

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PG proofs for the routing-prediction scorer (PR-3a), inference via a FAKE Inferer (deterministic,
// zero cost): skill margin, min-slice floor, author exclusion, bounded + inert.

// fakeInferer returns a canned answer per model and COUNTS calls (for the bounded-cost proof).
type fakeInferer struct {
	answers map[string]string
	calls   int64
}

func (f *fakeInferer) Infer(_ context.Context, model, _ string) (string, error) {
	atomic.AddInt64(&f.calls, 1)
	return f.answers[model], nil
}

// fakeBaseliner returns a fixed baseline model (or ok=false to simulate Advisor BasisNone).
type fakeBaseliner struct {
	model string
	ok    bool
}

func (b fakeBaseliner) Baseline(context.Context, string, string, string) (string, bool) {
	return b.model, b.ok
}

func on() func() bool  { return func() bool { return true } }
func off() func() bool { return func() bool { return false } }

func scoreHarness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG routing-score test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS routing_prediction_scores`,
		`DROP TABLE IF EXISTS routing_predictions`,
		`DROP TABLE IF EXISTS benchmark_eval_items`,
		`DROP TABLE IF EXISTS workspace_card_fingerprints`,
		`CREATE TABLE routing_predictions (id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text, workspace_id TEXT NOT NULL,
			feature_category TEXT NOT NULL, input_token_range TEXT NOT NULL, complexity_bucket TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE routing_prediction_scores (id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text, prediction_id TEXT NOT NULL UNIQUE,
			slice_size INTEGER NOT NULL, m_avg DOUBLE PRECISION NOT NULL, baseline_avg DOUBLE PRECISION NOT NULL,
			baseline_model TEXT NOT NULL, skill_margin DOUBLE PRECISION NOT NULL, scored_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE benchmark_eval_items (id TEXT PRIMARY KEY, input TEXT NOT NULL, expected_output TEXT NOT NULL,
			eval_method TEXT NOT NULL DEFAULT 'exact', active BOOLEAN NOT NULL DEFAULT TRUE, status TEXT NOT NULL DEFAULT 'active',
			author_workspace_id TEXT, feature_category TEXT, input_token_range TEXT, complexity_bucket TEXT)`,
		`CREATE TABLE workspace_card_fingerprints (workspace_id TEXT NOT NULL, fingerprint_hash TEXT NOT NULL, PRIMARY KEY (workspace_id, fingerprint_hash))`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func addPrediction(t *testing.T, pool *pgxpool.Pool, id, ws, feature, rng, cplx, model string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO routing_predictions (id, workspace_id, feature_category, input_token_range, complexity_bucket, model, status)
		 VALUES ($1,$2,$3,$4,$5,$6,'active')`, id, ws, feature, rng, cplx, model); err != nil {
		t.Fatal(err)
	}
}

func addItem(t *testing.T, pool *pgxpool.Pool, id, feature, rng, cplx, author string) {
	t.Helper()
	var a any
	if author != "" {
		a = author
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO benchmark_eval_items (id, input, expected_output, feature_category, input_token_range, complexity_bucket, author_workspace_id)
		 VALUES ($1,'in','RIGHT',$2,$3,$4,$5)`, id, feature, rng, cplx, a); err != nil {
		t.Fatal(err)
	}
}

func scoreOf(t *testing.T, pool *pgxpool.Pool, predID string) (skill float64, sliceSize int, present bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT skill_margin, slice_size FROM routing_prediction_scores WHERE prediction_id=$1`, predID).Scan(&skill, &sliceSize)
	if err != nil {
		return 0, 0, false
	}
	return skill, sliceSize, true
}

// (proof 1) Skill margin: M beats baseline -> >0; M equal -> ~0; M worse -> 0 (clamped).
func TestScore_SkillMargin_Integration(t *testing.T) {
	pool := scoreHarness(t)
	ctx := context.Background()
	for i, ids := range []string{"a", "b", "c", "d"} {
		addItem(t, pool, "item-"+ids, "summarize", "small", "simple", "")
		_ = i
	}
	// Baseline model "BASE" returns WRONG; predicted "M" returns RIGHT ⇒ M beats baseline.
	addPrediction(t, pool, "p-win", "wsA", "summarize", "small", "simple", "M")
	inf := &fakeInferer{answers: map[string]string{"M": "RIGHT", "BASE": "WRONG"}}
	sc := NewScorer(pool, fakeBaseliner{model: "BASE", ok: true}, inf, on())
	if n, err := sc.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("expected 1 scored: n=%d err=%v", n, err)
	}
	skill, sz, ok := scoreOf(t, pool, "p-win")
	if !ok || skill < 0.99 || sz != 4 {
		t.Fatalf("M-beats-baseline: skill=%.3f size=%d, want ~1.0/4", skill, sz)
	}

	// Equal: a NEW prediction whose model also returns RIGHT (== baseline behavior) ⇒ skill ~0.
	addPrediction(t, pool, "p-eq", "wsB", "summarize", "small", "simple", "EQ")
	inf2 := &fakeInferer{answers: map[string]string{"EQ": "RIGHT", "BASE": "RIGHT"}}
	if _, err := NewScorer(pool, fakeBaseliner{model: "BASE", ok: true}, inf2, on()).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if skill, _, ok := scoreOf(t, pool, "p-eq"); !ok || skill > 0.01 {
		t.Fatalf("M-equal-baseline: skill=%.3f, want ~0", skill)
	}

	// Worse: M returns WRONG, baseline RIGHT ⇒ clamped 0.
	addPrediction(t, pool, "p-lose", "wsC", "summarize", "small", "simple", "BAD")
	inf3 := &fakeInferer{answers: map[string]string{"BAD": "WRONG", "BASE": "RIGHT"}}
	if _, err := NewScorer(pool, fakeBaseliner{model: "BASE", ok: true}, inf3, on()).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if skill, _, ok := scoreOf(t, pool, "p-lose"); !ok || skill != 0 {
		t.Fatalf("M-worse-than-baseline: skill=%.3f, want 0 (clamped)", skill)
	}
}

// (proof 2) Min-slice floor: a cohort with < MinSliceSize tagged held items is unscored (no row). Also
// BasisNone baseline ⇒ unscored.
func TestScore_MinSliceAndNoBaseline_Integration(t *testing.T) {
	pool := scoreHarness(t)
	ctx := context.Background()
	// Only 2 items (< MinSliceSize=3) in the cohort.
	addItem(t, pool, "i1", "code", "medium", "moderate", "")
	addItem(t, pool, "i2", "code", "medium", "moderate", "")
	addPrediction(t, pool, "p-small", "wsA", "code", "medium", "moderate", "M")
	inf := &fakeInferer{answers: map[string]string{"M": "RIGHT", "BASE": "WRONG"}}
	if _, err := NewScorer(pool, fakeBaseliner{model: "BASE", ok: true}, inf, on()).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := scoreOf(t, pool, "p-small"); ok {
		t.Fatal("below MinSliceSize must be unscored (no row)")
	}
	if inf.calls != 0 {
		t.Fatalf("below-floor must run NO inference, got %d calls", inf.calls)
	}

	// No baseline (Advisor BasisNone) ⇒ unscored even with a full slice.
	for _, id := range []string{"j1", "j2", "j3"} {
		addItem(t, pool, id, "qa", "small", "simple", "")
	}
	addPrediction(t, pool, "p-nobase", "wsA", "qa", "small", "simple", "M")
	inf2 := &fakeInferer{answers: map[string]string{"M": "RIGHT"}}
	if _, err := NewScorer(pool, fakeBaseliner{ok: false}, inf2, on()).RunOnce(ctx); err != nil { // BasisNone
		t.Fatal(err)
	}
	if _, _, ok := scoreOf(t, pool, "p-nobase"); ok {
		t.Fatal("BasisNone baseline must be unscored")
	}
	if inf2.calls != 0 {
		t.Fatalf("no-baseline must run NO inference, got %d", inf2.calls)
	}
}

// (proof 3) Author exclusion: items authored by the predictor's workspace (+ fingerprint-linked set) are
// excluded from its slice.
func TestScore_AuthorExclusion_Integration(t *testing.T) {
	pool := scoreHarness(t)
	ctx := context.Background()
	// wsA authors 2 items; a linked sister wsB (shared card fp) authors 1; wsC (unlinked) authors 3.
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_card_fingerprints VALUES ('wsA','fp1'),('wsB','fp1')`); err != nil {
		t.Fatal(err)
	}
	addItem(t, pool, "own1", "summarize", "small", "simple", "wsA")
	addItem(t, pool, "own2", "summarize", "small", "simple", "wsA")
	addItem(t, pool, "sis1", "summarize", "small", "simple", "wsB") // linked sister
	addItem(t, pool, "ext1", "summarize", "small", "simple", "wsC")
	addItem(t, pool, "ext2", "summarize", "small", "simple", "wsC")
	addItem(t, pool, "ext3", "summarize", "small", "simple", "wsC")
	addPrediction(t, pool, "p-excl", "wsA", "summarize", "small", "simple", "M")
	inf := &fakeInferer{answers: map[string]string{"M": "RIGHT", "BASE": "WRONG"}}
	if _, err := NewScorer(pool, fakeBaseliner{model: "BASE", ok: true}, inf, on()).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	_, sz, ok := scoreOf(t, pool, "p-excl")
	if !ok || sz != 3 {
		t.Fatalf("slice must exclude wsA's own (2) + linked wsB (1), leaving wsC's 3: got size=%d ok=%v", sz, ok)
	}
}

// (proof 4) Bounded + inert: flag-OFF ⇒ the fake Inferer is NEVER called (zero inference). And the
// worst-case call count is ≤ BatchLimit*SliceCap*2.
func TestScore_BoundedAndInert_Integration(t *testing.T) {
	pool := scoreHarness(t)
	ctx := context.Background()
	for _, id := range []string{"i1", "i2", "i3"} {
		addItem(t, pool, id, "summarize", "small", "simple", "")
	}
	addPrediction(t, pool, "p1", "wsA", "summarize", "small", "simple", "M")

	// Flag OFF ⇒ zero inference, nothing scored.
	infOff := &fakeInferer{answers: map[string]string{"M": "RIGHT", "BASE": "WRONG"}}
	if n, _ := NewScorer(pool, fakeBaseliner{model: "BASE", ok: true}, infOff, off()).RunOnce(ctx); n != 0 || infOff.calls != 0 {
		t.Fatalf("flag-off must be inert: scored=%d calls=%d, want 0/0", n, infOff.calls)
	}
	// Nil Inferer ⇒ inert even with flag ON.
	if n, _ := NewScorer(pool, fakeBaseliner{model: "BASE", ok: true}, nil, on()).RunOnce(ctx); n != 0 {
		t.Fatalf("nil Inferer must be inert even flag-on, scored=%d", n)
	}
	// Flag ON + real fake Inferer ⇒ bounded call count.
	infOn := &fakeInferer{answers: map[string]string{"M": "RIGHT", "BASE": "WRONG"}}
	if _, err := NewScorer(pool, fakeBaseliner{model: "BASE", ok: true}, infOn, on()).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if infOn.calls > int64(BatchLimit*SliceCap*2) {
		t.Fatalf("call count %d exceeds worst-case BatchLimit*SliceCap*2=%d", infOn.calls, BatchLimit*SliceCap*2)
	}
	if infOn.calls != 6 { // 1 prediction × 3 slice items × 2 models (M + baseline)
		t.Fatalf("expected 6 calls (3 items × M+baseline), got %d", infOn.calls)
	}
}
