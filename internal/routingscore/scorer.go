package routingscore

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/eval"
	"github.com/talyvor/lens/internal/routing"
)

// Bounds (locked decision c) — the cost envelope. Flag-off OR a nil Inferer ⇒ the sweeper runs no
// inference at all.
const (
	MinSliceSize = 3  // a cohort with fewer tagged held items is UNSCORED (untrustworthy — the warmup pattern)
	SliceCap     = 20 // at most this many held items scored per cohort (bounds inference per prediction)
	BatchLimit   = 20 // at most this many predictions scored per sweep tick
)

// Inferer runs a model on an input and returns the response text. PR-3a ships NO live implementation —
// the real provider-backed Inferer arrives in PR-3b (the Option-Y internal/inference extraction). A nil
// Inferer makes the scorer provably inert (no inference, no score).
type Inferer interface {
	Infer(ctx context.Context, model, input string) (string, error)
}

// Baseliner resolves the baseline default route for a cohort. ok=false ⇒ no single resolvable default
// (Advisor BasisNone) ⇒ the prediction is UNSCORED. The real adapter wraps *routing.Advisor.
type Baseliner interface {
	Baseline(ctx context.Context, feature, inputTokenRange, complexity string) (model string, ok bool)
}

// advisorBaseliner adapts *routing.Advisor.RecommendByRange to Baseliner. BasisNone ⇒ ok=false.
type advisorBaseliner struct{ adv *routing.Advisor }

// NewAdvisorBaseliner wraps the live routing Advisor as the baseline resolver (used in main wiring).
func NewAdvisorBaseliner(adv *routing.Advisor) Baseliner { return advisorBaseliner{adv: adv} }

func (b advisorBaseliner) Baseline(ctx context.Context, feature, inputTokenRange, _ string) (string, bool) {
	rec := b.adv.RecommendByRange(ctx, "", feature, inputTokenRange, "", nil, nil)
	if rec.Basis == routing.BasisNone || rec.Model == "" {
		return "", false // no single resolvable default route for this cohort ⇒ unscored
	}
	return rec.Model, true
}

// Scorer is the leader-elected, flag-gated routing-prediction scorer. INERT when the flag is off OR the
// Inferer is nil (PR-3a wires no real Inferer): RunOnce then does no DB scan and no inference.
type Scorer struct {
	st        *store
	baseliner Baseliner
	inferer   Inferer
	enabled   func() bool
}

// NewScorer wires the pool, the baseline resolver, the Inferer (nil ⇒ inert), and the capability flag.
func NewScorer(pool *pgxpool.Pool, baseliner Baseliner, inferer Inferer, enabled func() bool) *Scorer {
	return &Scorer{st: &store{pool: pool}, baseliner: baseliner, inferer: inferer, enabled: enabled}
}

// RunOnce scores up to BatchLimit active, not-yet-scored predictions. TOTAL no-op (no DB, no inference)
// when the flag is off, the Inferer is nil, or deps are missing — the PR-3a inert guarantee.
func (s *Scorer) RunOnce(ctx context.Context) (int, error) {
	if s == nil || s.inferer == nil || s.enabled == nil || !s.enabled() || s.st == nil || s.baseliner == nil {
		return 0, nil // INERT: no real Inferer (PR-3a) or flag off ⇒ zero inference
	}
	preds, err := s.st.activeUnscored(ctx, BatchLimit)
	if err != nil {
		return 0, err
	}
	scored := 0
	for _, p := range preds {
		ok, err := s.scoreOne(ctx, p)
		if err != nil {
			slog.Warn("routingscore: score failed (prediction stays unscored; retries next tick)",
				slog.String("prediction", p.ID), slog.String("error", err.Error()))
			continue
		}
		if ok {
			scored++
		}
	}
	return scored, nil
}

// scoreOne resolves the baseline, builds the author-excluded held slice (≥MinSliceSize, ≤SliceCap), runs
// M and the baseline on it, and stores skill_margin = clamp01(avg(M) − avg(baseline)). Returns
// (false,nil) for an unscorable prediction (no baseline / slice too small) — no row, re-eligible.
func (s *Scorer) scoreOne(ctx context.Context, p predictionRow) (bool, error) {
	baselineModel, ok := s.baseliner.Baseline(ctx, p.FeatureCategory, p.InputTokenRange, p.ComplexityBucket)
	if !ok {
		return false, nil // BasisNone — no resolvable default to beat ⇒ unscored
	}
	// A prediction that just restates the baseline can never beat it — margin 0 by definition; skip the
	// inference entirely (a cost saving, not a correctness change: clamp01(x−x)=0).
	if baselineModel == p.Model {
		return false, nil
	}
	slice, err := s.st.cohortSlice(ctx, p.FeatureCategory, p.InputTokenRange, p.ComplexityBucket, p.WorkspaceID, SliceCap)
	if err != nil {
		return false, err
	}
	if len(slice) < MinSliceSize {
		return false, nil // warmup floor — too few held items to trust ⇒ unscored
	}

	mAvg, err := s.runAndScore(ctx, p.Model, slice)
	if err != nil {
		return false, err
	}
	baselineAvg, err := s.runAndScore(ctx, baselineModel, slice)
	if err != nil {
		return false, err
	}
	skill := mAvg - baselineAvg
	if skill < 0 {
		skill = 0 // clamp01 lower: M does not beat the baseline ⇒ no skill
	}
	if skill > 1 {
		skill = 1
	}
	if err := s.st.insertScore(ctx, p.ID, len(slice), mAvg, baselineAvg, baselineModel, skill); err != nil {
		return false, err
	}
	return true, nil
}

// runAndScore runs `model` on every slice item via the Inferer and returns the average held-truth score
// (eval.StaticScore vs expected_output). This is the ONLY producer the score reads — never output_quality.
func (s *Scorer) runAndScore(ctx context.Context, model string, slice []sliceItem) (float64, error) {
	var sum float64
	for _, it := range slice {
		answer, err := s.inferer.Infer(ctx, model, it.Input)
		if err != nil {
			return 0, err
		}
		score, _, serr := eval.StaticScore(eval.EvalMethod(it.EvalMethod), it.ExpectedOutput, answer)
		if serr != nil {
			return 0, serr
		}
		sum += score
	}
	return sum / float64(len(slice)), nil
}

// StartScheduler ticks RunOnce until ctx ends (leader-elected at the call site). Inert until both the
// flag is on AND a real Inferer is wired (PR-3b).
func (s *Scorer) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.RunOnce(ctx); err != nil {
				slog.Warn("routingscore: scoring sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}
