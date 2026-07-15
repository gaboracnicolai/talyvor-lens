package routingbrain

import (
	"context"
	"testing"
)

// fakeSrc is a canned recStore for the in-memory Brain tests (no DB).
type fakeSrc struct {
	recs []Recommendation
	auto []string
}

func (f fakeSrc) LoadRecommendations(context.Context) ([]Recommendation, error) { return f.recs, nil }
func (f fakeSrc) LoadAutonomous(context.Context) ([]string, error)              { return f.auto, nil }

// fixedCost: "pricey" is expensive, everything else is cheap.
func fixedCost(m string) float64 {
	if m == "pricey" {
		return 10.0
	}
	return 1.0
}

func loadedBrain(t *testing.T, src fakeSrc) *Brain {
	t.Helper()
	b := New(src, fixedCost, Config{Enabled: true})
	if err := b.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return b
}

// TestBrain_Refresh_LookupAndMode — Refresh loads the recommendations + autonomous
// set into memory; Lookup is an in-memory hit; ModeFor reflects the opt-in.
func TestBrain_Refresh_LookupAndMode(t *testing.T) {
	b := loadedBrain(t, fakeSrc{
		recs: []Recommendation{{WorkspaceID: "ws1", Difficulty: 2, Model: "A", Verified: true}},
		auto: []string{"wsAuto"},
	})
	if rec, ok := b.Lookup("ws1", 2); !ok || rec.Model != "A" {
		t.Errorf("Lookup(ws1,2) miss or wrong: %v ok=%v", rec, ok)
	}
	if _, ok := b.Lookup("ws1", 5); ok {
		t.Error("Lookup of an uncomputed cohort must miss")
	}
	if b.ModeFor("wsAuto") != ModeAutonomous {
		t.Error("opted-in workspace must be autonomous")
	}
	if b.ModeFor("ws1") != ModeAdvisory {
		t.Error("non-opted-in workspace must default to advisory")
	}
}

// TestBrain_Resolve_AdvisoryVsAutonomous — the serve-time entry: advisory keeps the
// safe model; autonomous applies the (verified, allowed, not-pricier) brain pick.
func TestBrain_Resolve_AdvisoryVsAutonomous(t *testing.T) {
	src := fakeSrc{
		recs: []Recommendation{
			{WorkspaceID: "wsAdv", Difficulty: 1, Model: "brainpick", Provider: "p", Verified: true, Reason: "x"},
			{WorkspaceID: "wsAuto", Difficulty: 1, Model: "brainpick", Provider: "p", Verified: true, Reason: "x"},
		},
		auto: []string{"wsAuto"},
	}
	b := loadedBrain(t, src)
	allowed := []string{"brainpick", "router-safe"}

	// Advisory workspace → route stays the router's safe model.
	if d, ok := b.Resolve("wsAdv", 1, "router-safe", allowed); !ok || d.Model != "router-safe" || d.Applied {
		t.Errorf("advisory Resolve must keep safe model unchanged; got %+v ok=%v", d, ok)
	}
	// Autonomous workspace, floor ok (brainpick cheap + verified + allowed) → applied.
	if d, ok := b.Resolve("wsAuto", 1, "router-safe", allowed); !ok || d.Model != "brainpick" || !d.Applied {
		t.Errorf("autonomous Resolve must apply the brain pick; got %+v ok=%v", d, ok)
	}
	// Autonomous but the brain pick is pricier than safe → hard floor → fallback.
	pricey := fakeSrc{recs: []Recommendation{{WorkspaceID: "wsAuto", Difficulty: 1, Model: "pricey", Verified: true}}, auto: []string{"wsAuto"}}
	bp := loadedBrain(t, pricey)
	if d, ok := bp.Resolve("wsAuto", 1, "router-safe", []string{"pricey", "router-safe"}); !ok || d.Model != "router-safe" || d.Applied {
		t.Errorf("autonomous with pricier pick must fall back (cost floor); got %+v ok=%v", d, ok)
	}
}

// TestBrain_Disabled_NeverResolves — a disabled brain never resolves (serving path
// reads no recommendation) and never refreshes state.
func TestBrain_Disabled_NeverResolves(t *testing.T) {
	b := New(fakeSrc{recs: []Recommendation{{WorkspaceID: "ws1", Difficulty: 0, Model: "A", Verified: true}}, auto: []string{"ws1"}}, fixedCost, Config{Enabled: false})
	_ = b.Refresh(context.Background()) // even if called, disabled Resolve must not act
	if _, ok := b.Resolve("ws1", 0, "safe", []string{"A"}); ok {
		t.Error("disabled brain must not resolve")
	}
	if b.Enabled() {
		t.Error("Enabled() must be false")
	}
}

// TestBrain_NilSafe_And_NoRec — resolving an unknown cohort misses cleanly.
func TestBrain_NoRec_Misses(t *testing.T) {
	b := loadedBrain(t, fakeSrc{})
	if _, ok := b.Resolve("nobody", 3, "safe", nil); ok {
		t.Error("no recommendation → Resolve must miss (ok=false)")
	}
}
