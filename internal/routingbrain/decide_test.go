package routingbrain

import "testing"

// safe is the model the existing router would use WITHOUT the brain, priced 1.0.
func safe() SafeDecision { return SafeDecision{Model: "router-safe", Cost: 1.0} }

// a verified, allowed, cheaper recommendation for a DIFFERENT model than the safe pick.
func goodRec() *Recommendation {
	return &Recommendation{WorkspaceID: "ws1", Difficulty: 3, Model: "brain-pick", Provider: "openai",
		ExpectedQuality: 0.9, Verified: true, Reason: "best verified q at tier 3"}
}

// TestDecide_Advisory_NeverChangesRoute — the headline advisory guarantee: in
// ADVISORY mode the chosen model is ALWAYS the router's safe pick, NEVER the
// brain's — even when the brain has a perfectly valid, cheaper, verified pick. The
// recommendation is only SURFACED.
func TestDecide_Advisory_NeverChangesRoute(t *testing.T) {
	d := Decide(ModeAdvisory, goodRec(), safe(), 0.5 /*rec cheaper*/, []string{"brain-pick", "router-safe"})
	if d.Model != "router-safe" {
		t.Errorf("advisory MUST route the router's safe model; got %q, want %q", d.Model, "router-safe")
	}
	if d.Applied {
		t.Error("advisory MUST NOT apply the brain's pick")
	}
	if !d.Surfaced {
		t.Error("advisory should still surface the recommendation as advice")
	}
}

// TestDecide_Autonomous_AppliesBrainPick — in AUTONOMOUS mode, when the hard floor
// passes (verified + allowed + not pricier than the safe pick), the brain's model
// becomes the route.
func TestDecide_Autonomous_AppliesBrainPick(t *testing.T) {
	d := Decide(ModeAutonomous, goodRec(), safe(), 0.5 /*cheaper*/, []string{"brain-pick", "router-safe"})
	if d.Model != "brain-pick" {
		t.Errorf("autonomous+floor-ok MUST apply the brain pick; got %q, want %q", d.Model, "brain-pick")
	}
	if !d.Applied {
		t.Error("autonomous+floor-ok must mark Applied")
	}
}

// TestDecide_HardFloor_OverridesAutonomous — THE floor proof. The brain picks a
// model that BOTH (i) breaches the cost cap (pricier than the safe pick) AND (ii)
// is unverified/unsafe (Keel-unverified and not in the allow-list). Autonomous
// notwithstanding, the route MUST fall back to the safe decision.
func TestDecide_HardFloor_OverridesAutonomous(t *testing.T) {
	bad := &Recommendation{WorkspaceID: "ws1", Difficulty: 3, Model: "reckless-pick", Provider: "x",
		ExpectedQuality: 0.99, Verified: false, Reason: "looks great but unsafe"}
	d := Decide(ModeAutonomous, bad, safe(), 5.0 /*5x pricier → breaches cap*/, []string{"router-safe"} /*bad not allowed*/)
	if d.Model != "router-safe" {
		t.Errorf("HARD FLOOR breach MUST fall back to the safe model; got %q, want %q", d.Model, "router-safe")
	}
	if d.Applied {
		t.Error("a floor breach must NOT apply the brain pick")
	}
}

// TestDecide_HardFloor_EachConditionAlone — each floor condition is INDEPENDENTLY
// sufficient to force fallback: unverified alone, not-allowed alone, over-cost alone.
func TestDecide_HardFloor_EachConditionAlone(t *testing.T) {
	allowed := []string{"brain-pick", "router-safe"}
	for _, c := range []struct {
		name          string
		verified      bool
		recCost       float64
		allowedModels []string
	}{
		{"unverified only", false, 0.5, allowed},
		{"not-allowed only", true, 0.5, []string{"router-safe"}},
		{"over-cost only", true, 1.5, allowed},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := goodRec()
			rec.Verified = c.verified
			d := Decide(ModeAutonomous, rec, safe(), c.recCost, c.allowedModels)
			if d.Model != "router-safe" || d.Applied {
				t.Errorf("%s must fall back to safe; got model=%q applied=%v", c.name, d.Model, d.Applied)
			}
		})
	}
}

// TestDecide_NoRecommendation_RoutesSafe — with no recommendation there is nothing
// to surface or apply; the safe decision stands.
func TestDecide_NoRecommendation_RoutesSafe(t *testing.T) {
	for _, rec := range []*Recommendation{nil, {Model: ""}} {
		d := Decide(ModeAutonomous, rec, safe(), 0.1, []string{"anything"})
		if d.Model != "router-safe" || d.Applied || d.Surfaced {
			t.Errorf("no-rec must route safe and surface nothing; got %+v", d)
		}
	}
}

// TestDecide_Autonomous_EqualCostAllowed — the cap is "not MORE expensive"; an
// equal-cost verified pick is applied (re-rank at same price toward higher quality).
func TestDecide_Autonomous_EqualCostAllowed(t *testing.T) {
	d := Decide(ModeAutonomous, goodRec(), safe(), 1.0 /*equal to safe*/, []string{"brain-pick", "router-safe"})
	if d.Model != "brain-pick" || !d.Applied {
		t.Errorf("equal-cost verified pick must apply; got model=%q applied=%v", d.Model, d.Applied)
	}
}
