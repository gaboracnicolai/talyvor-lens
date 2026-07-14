package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/cohort"
	"github.com/talyvor/lens/internal/routingpredict"
)

type fakeRoutingEmitter struct {
	calls int
	last  routingpredict.Prediction
	err   error
}

func (f *fakeRoutingEmitter) EmitLivePrediction(_ context.Context, p routingpredict.Prediction) (string, error) {
	f.calls++
	f.last = p
	return "pred-id", f.err
}

// THE FARM GATE: a request where cohort intelligence did NOT override the baseline (trivial / no
// applicable cohort) emits NO prediction — so it can never reach the scorer or mint. This is the emit-level
// rarity floor (the scorer's MinSliceSize + skill<0→0 clamp is the second floor downstream).
func TestEmitRoutingPrediction_NotApplied_NoEmit(t *testing.T) {
	em := &fakeRoutingEmitter{}
	p := &Proxy{routingPredictEmitter: em, routingPredictEmitEnabled: func() bool { return true }}
	p.emitRoutingPrediction(context.Background(), "ws1", "chat", "gpt-4o", "openai",
		false /* cohortOverrode */, 100, "simple")
	if em.calls != 0 {
		t.Errorf("cohort did NOT override baseline: emitter called %d times, want 0 (farm gate: trivial requests emit nothing)", em.calls)
	}
}

// Disabled capability flag → total no-op (the table stays provably empty until deliberately enabled).
func TestEmitRoutingPrediction_Disabled_NoEmit(t *testing.T) {
	em := &fakeRoutingEmitter{}
	p := &Proxy{routingPredictEmitter: em, routingPredictEmitEnabled: func() bool { return false }}
	p.emitRoutingPrediction(context.Background(), "ws1", "chat", "gpt-4o", "openai", true, 100, "simple")
	if em.calls != 0 {
		t.Errorf("disabled: emitter called %d times, want 0", em.calls)
	}
}

// A missing model or feature (cohort key incomplete) → no emit (validate would reject it anyway).
func TestEmitRoutingPrediction_IncompleteCohort_NoEmit(t *testing.T) {
	em := &fakeRoutingEmitter{}
	p := &Proxy{routingPredictEmitter: em, routingPredictEmitEnabled: func() bool { return true }}
	p.emitRoutingPrediction(context.Background(), "ws1", "chat", "" /* no model */, "openai", true, 100, "simple")
	p.emitRoutingPrediction(context.Background(), "ws1", "" /* no feature */, "gpt-4o", "openai", true, 100, "simple")
	if em.calls != 0 {
		t.Errorf("incomplete cohort: emitter called %d times, want 0", em.calls)
	}
}

// OFF-PATH: an emitter error (incl. the expected duplicate) is swallowed — emitRoutingPrediction returns
// normally (the response is already flushed; a prediction-record failure must never surface to serving).
func TestEmitRoutingPrediction_OffPath_SwallowsError(t *testing.T) {
	for _, e := range []error{errors.New("db down"), routingpredict.ErrDuplicatePrediction} {
		em := &fakeRoutingEmitter{err: e}
		p := &Proxy{routingPredictEmitter: em, routingPredictEmitEnabled: func() bool { return true }}
		p.emitRoutingPrediction(context.Background(), "ws1", "chat", "gpt-4o", "openai", true, 100, "simple")
		if em.calls != 1 {
			t.Errorf("emitter called %d times, want 1 (error %v swallowed, not propagated)", em.calls, e)
		}
	}
}

// The emitted prediction carries the LIVE decision's cohort (feature + canonical input/complexity buckets +
// the cohort-recommended model) and status is left to EmitLivePrediction (→ 'active', the live assertion).
// This binds the emit cohort to cohort.DeriveInputCohort so a prediction and its held-eval slice share a
// cohort (else it would be scored against the wrong slice / never).
func TestEmitRoutingPrediction_CarriesLiveCohort(t *testing.T) {
	em := &fakeRoutingEmitter{}
	p := &Proxy{routingPredictEmitter: em, routingPredictEmitEnabled: func() bool { return true }}
	const input = "What is the capital of France?"
	wantRange, wantComplexity := cohort.DeriveInputCohort(input)
	p.emitRoutingPrediction(context.Background(), "ws1", "chat", "gpt-4o", "openai",
		true, len(input)/4, wantComplexity)
	if em.calls != 1 {
		t.Fatalf("emitter called %d times, want 1", em.calls)
	}
	got := em.last
	if got.WorkspaceID != "ws1" || got.FeatureCategory != "chat" || got.Model != "gpt-4o" || got.Provider != "openai" {
		t.Errorf("prediction identity wrong: %+v", got)
	}
	if got.InputTokenRange != wantRange {
		t.Errorf("InputTokenRange = %q, want %q (must equal cohort.DeriveInputCohort so the held-eval slice resolves)", got.InputTokenRange, wantRange)
	}
	if got.ComplexityBucket != wantComplexity {
		t.Errorf("ComplexityBucket = %q, want %q", got.ComplexityBucket, wantComplexity)
	}
}
