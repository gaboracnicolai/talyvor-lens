package proxy

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/routingbrain"
)

// fakeBrainSrc builds a real *routingbrain.Brain in-package for the proxy wiring test.
type fakeBrainSrc struct {
	recs []routingbrain.Recommendation
	auto []string
}

func (f fakeBrainSrc) LoadRecommendations(context.Context) ([]routingbrain.Recommendation, error) {
	return f.recs, nil
}
func (f fakeBrainSrc) LoadAutonomous(context.Context) ([]string, error) { return f.auto, nil }

// brainCost: "pricey" is expensive, everything else cheap.
func brainCost(m string) float64 {
	if m == "pricey" {
		return 10.0
	}
	return 1.0
}

func buildProxyBrain(t *testing.T, recs []routingbrain.Recommendation, auto []string) *routingbrain.Brain {
	t.Helper()
	b := routingbrain.New(fakeBrainSrc{recs: recs, auto: auto}, brainCost, routingbrain.Config{Enabled: true})
	if err := b.Refresh(context.Background()); err != nil {
		t.Fatalf("brain refresh: %v", err)
	}
	return b
}

// large+complex request → work-tier difficulty 5 (size rank 2 + complexity rank 3).
const brainInTok, brainCplx, brainDiff = 50000, 5, 5

// TestApplyRoutingBrain_Advisory_ByteKeepsRouterModel — advisory mode must NEVER
// change the route: applyRoutingBrain returns the ROUTER's safe model, not the
// brain's, even with a valid cheaper verified pick present. Byte-verified.
func TestApplyRoutingBrain_Advisory_ByteKeepsRouterModel(t *testing.T) {
	brain := buildProxyBrain(t,
		[]routingbrain.Recommendation{{WorkspaceID: "ws1", Difficulty: brainDiff, Model: "brain-pick", Verified: true, Reason: "r"}},
		nil /* not autonomous → advisory */)
	p := &Proxy{routingBrain: brain}
	model, _, applied, surfaced := p.applyRoutingBrain("ws1", brainInTok, brainCplx, "router-safe", []string{"brain-pick", "router-safe"})
	if model != "router-safe" {
		t.Errorf("advisory MUST keep the router's model; got %q want %q", model, "router-safe")
	}
	if applied {
		t.Error("advisory must not apply the brain pick")
	}
	if !surfaced {
		t.Error("advisory should surface the recommendation")
	}
}

// TestApplyRoutingBrain_Autonomous_AppliesBrainPick — autonomous + floor-ok applies
// the brain's model.
func TestApplyRoutingBrain_Autonomous_AppliesBrainPick(t *testing.T) {
	brain := buildProxyBrain(t,
		[]routingbrain.Recommendation{{WorkspaceID: "wsA", Difficulty: brainDiff, Model: "brain-pick", Verified: true, Reason: "r"}},
		[]string{"wsA"})
	p := &Proxy{routingBrain: brain}
	model, _, applied, _ := p.applyRoutingBrain("wsA", brainInTok, brainCplx, "router-safe", []string{"brain-pick", "router-safe"})
	if model != "brain-pick" || !applied {
		t.Errorf("autonomous+floor-ok MUST apply the brain pick; got model=%q applied=%v", model, applied)
	}
}

// TestApplyRoutingBrain_Autonomous_HardFloorFallsBack — autonomous but the brain
// pick breaches the floor (pricier + unverified + not allowed) → the route falls
// back to the router's safe model.
func TestApplyRoutingBrain_Autonomous_HardFloorFallsBack(t *testing.T) {
	brain := buildProxyBrain(t,
		[]routingbrain.Recommendation{{WorkspaceID: "wsA", Difficulty: brainDiff, Model: "pricey", Verified: false, Reason: "unsafe"}},
		[]string{"wsA"})
	p := &Proxy{routingBrain: brain}
	model, _, applied, _ := p.applyRoutingBrain("wsA", brainInTok, brainCplx, "router-safe", []string{"router-safe"} /* pricey not allowed */)
	if model != "router-safe" || applied {
		t.Errorf("hard-floor breach MUST fall back to safe; got model=%q applied=%v", model, applied)
	}
}

// TestApplyRoutingBrain_OffOrNoRec_NoEffect — a nil/disabled brain, or a cohort with
// no recommendation, leaves the route untouched and surfaces nothing.
func TestApplyRoutingBrain_OffOrNoRec_NoEffect(t *testing.T) {
	// nil brain
	pNil := &Proxy{}
	if m, _, applied, surfaced := pNil.applyRoutingBrain("ws1", brainInTok, brainCplx, "router-safe", nil); m != "router-safe" || applied || surfaced {
		t.Errorf("nil brain must be a no-op; got model=%q applied=%v surfaced=%v", m, applied, surfaced)
	}
	// enabled brain but no rec for this cohort
	brain := buildProxyBrain(t, nil, nil)
	p := &Proxy{routingBrain: brain}
	if m, _, applied, surfaced := p.applyRoutingBrain("ws1", brainInTok, brainCplx, "router-safe", nil); m != "router-safe" || applied || surfaced {
		t.Errorf("no-rec must be a no-op; got model=%q applied=%v surfaced=%v", m, applied, surfaced)
	}
}
