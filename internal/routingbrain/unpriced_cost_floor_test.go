package routingbrain

import (
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/catalog"
)

// unpriced_cost_floor_test.go — what the brain's HARD COST FLOOR does when it
// cannot price a model.
//
// ⚠ NOTHING IS FIXED HERE AND THAT IS THE POINT OF THE ITEM (W6.20). The direction
// of the repair is a decision about what an unpriced model should COST on this
// surface, and unlike W6.16 (#484, a settle beside a settle) and W6.19 (#487, a gate
// beside a gate) there is no twin on this path that has already decided it. Naming
// the wrong direction here would flip which model the brain routes to.
//
// THE MECHANISM, from main.go: the brain's cost function is
//
//	brainCost := func(m string) float64 { return alerts.CostUSD(m, 1000, 1000) }
//
// and alerts.CostUSD returns EXACTLY ZERO for any model the 45-entry catalog does
// not hold (W6.16). The catalog does not hold `gpt-4`, `gpt-4-turbo` or
// `claude-3-opus`. That zero is then used in two places that decide routing:
//
//	floorOK:  `if recCost > safe.Cost { return false }`   — the hard cost cap
//	better(): ties on quality break to `a.cost < b.cost`  — the candidate pick
//
// so an unpriced model reads as free to both. The three tests below drive each
// consequence. They assert TODAY'S behaviour, deliberately, and each says what a
// decision would change — a test that pinned the fix would be pinning a price
// nobody chose.

const (
	brainUnpricedModel = "gpt-4"  // absent from the catalog
	brainPricedModel   = "gpt-4o" // present
)

// brainCost mirrors main.go's wiring exactly, so these tests measure the shipped
// basis rather than a convenient one.
func brainCostFn(m string) float64 { return alerts.CostUSD(m, 1000, 1000) }

func TestBrainCostBasis_PremiseHolds(t *testing.T) {
	if _, _, ok := catalog.Price(brainUnpricedModel); ok {
		t.Fatalf("%s is now in the catalog — every test in this file was written around it "+
			"being absent", brainUnpricedModel)
	}
	if brainCostFn(brainUnpricedModel) != 0 {
		t.Fatalf("the brain's cost basis no longer returns 0 for %q — W6.20's premise has "+
			"changed and its classification needs re-measuring", brainUnpricedModel)
	}
	if brainCostFn(brainPricedModel) <= 0 {
		t.Fatalf("the priced control prices at 0 — it is not a control")
	}
}

// ⚠ CONSEQUENCE 1 — THE HARD FLOOR CANNOT REFUSE AN UNPRICED MODEL.
// floorOK rejects a recommendation whose cost exceeds the safe decision's. An
// unpriced model costs 0, so it clears the cap however expensive it really is, and
// Decide applies it autonomously.
func TestUnpricedRecommendationAlwaysClearsTheCostFloor(t *testing.T) {
	safe := SafeDecision{Model: brainPricedModel, Cost: brainCostFn(brainPricedModel)}
	if safe.Cost <= 0 {
		t.Fatal("the safe model prices at 0 — the cap would be trivially unclearable and this " +
			"test would prove nothing")
	}
	rec := &Recommendation{Model: brainUnpricedModel, Verified: true, Reason: "probe"}
	recCost := brainCostFn(brainUnpricedModel)

	d := Decide(ModeAutonomous, rec, safe, recCost, []string{brainUnpricedModel})
	if !d.Applied {
		t.Fatalf("the recommendation was not applied (%+v) — then this test is not measuring the "+
			"cost floor", d)
	}
	t.Logf("MEASURED: %q (unpriced, cost basis %v) cleared a cost cap set by %q (%v) and was "+
		"applied autonomously. The cap compares two numbers and one of them is structurally zero.",
		rec.Model, recCost, safe.Model, safe.Cost)

	// And the same recommendation on a model the catalog DOES price, at a genuinely
	// higher cost, is refused — which is the cap working, and the contrast that makes
	// the finding a finding rather than a description of a cap that never fires.
	pricier := &Recommendation{Model: brainPricedModel, Verified: true, Reason: "probe"}
	refused := Decide(ModeAutonomous, pricier, SafeDecision{Model: "cheap-safe", Cost: safe.Cost / 2},
		safe.Cost, []string{brainPricedModel})
	if refused.Applied {
		t.Error("a priced recommendation above the cap was applied — the cap does not fire at " +
			"all, so consequence 1 is not about unpriced models")
	}
}

// ⚠ CONSEQUENCE 2 — THE CANDIDATE PICK PREFERS MODELS IT CANNOT PRICE.
// better() breaks a quality tie on `a.cost < b.cost`, and an unpriced candidate is
// 0. Among equal-quality candidates the unpriced one therefore always wins.
func TestUnpricedCandidateWinsEveryQualityTie(t *testing.T) {
	// ⚠ THE PRICED MODEL HERE SORTS BEFORE THE UNPRICED ONE, AND THAT IS THE WHOLE
	// DESIGN OF THE TEST. better() falls through from cost to `a.model < b.model`, so
	// a pair like (gpt-4, gpt-4o) cannot distinguish "the unpriced one won because it
	// looked free" from "it won because it sorts first" — control M6 disabled the cost
	// comparison entirely and the first draft of this test still passed.
	// "claude-sonnet-4-6" (priced) sorts BEFORE "gpt-4" (unpriced), so the name
	// tie-break would pick the PRICED one. Only the cost comparison can produce the
	// result asserted below.
	const pricedEarlierName = "claude-sonnet-4-6"
	if _, _, ok := catalog.Price(pricedEarlierName); !ok {
		t.Fatalf("%s is not in the catalog — the discriminating pair no longer discriminates",
			pricedEarlierName)
	}
	if pricedEarlierName >= brainUnpricedModel {
		t.Fatalf("%q does not sort before %q — the name tie-break would agree with the cost one "+
			"and this test could not tell them apart", pricedEarlierName, brainUnpricedModel)
	}

	unpriced := candidate{model: brainUnpricedModel, quality: 0.9, cost: brainCostFn(brainUnpricedModel)}
	priced := candidate{model: pricedEarlierName, quality: 0.9, cost: brainCostFn(pricedEarlierName)}
	if priced.cost <= unpriced.cost {
		t.Fatalf("the priced candidate (%v) is not dearer than the unpriced one (%v) — the tie "+
			"break below would be measuring nothing", priced.cost, unpriced.cost)
	}

	if !better(unpriced, priced) {
		t.Error("better() did not prefer the unpriced candidate — the premise has changed")
	}
	if better(priced, unpriced) {
		t.Error("better() preferred the priced candidate over the free-looking one")
	}
	t.Logf("MEASURED: on a quality tie the brain prefers %q (unpriced, basis %v) over %q "+
		"(basis %v) — and %q would have won on name alone, so the preference is the COST "+
		"comparison and nothing else.",
		unpriced.model, unpriced.cost, priced.model, priced.cost, priced.model)
}

// ⚠ CONSEQUENCE 3 — AND IT INVERTS WHEN THE *SAFE* MODEL IS THE UNPRICED ONE.
// safe.Cost is 0, so every priced recommendation "exceeds the cost cap" and the
// brain falls back to the safe model every time. The same zero blocks in one
// direction and waves through in the other, which is why "just use a fallback" is
// not obviously the right repair and why W6.20 does not pick one.
func TestUnpricedSafeModelRefusesEveryPricedRecommendation(t *testing.T) {
	safe := SafeDecision{Model: brainUnpricedModel, Cost: brainCostFn(brainUnpricedModel)}
	rec := &Recommendation{Model: brainPricedModel, Verified: true, Reason: "probe"}
	recCost := brainCostFn(brainPricedModel)
	if recCost <= 0 {
		t.Fatal("the priced recommendation costs 0 — nothing to exceed")
	}

	d := Decide(ModeAutonomous, rec, safe, recCost, []string{brainPricedModel})
	if d.Applied {
		t.Error("a priced recommendation was applied over an unpriced safe model — the premise " +
			"has changed")
	}
	if got := floorViolation(rec, safe, recCost, []string{brainPricedModel}); got != "exceeds cost cap (safe decision)" {
		t.Errorf("floorViolation = %q, want the cost-cap reason — the refusal is happening for a "+
			"different cause and this test is mislabelled", got)
	}
	t.Logf("MEASURED: with %q (unpriced, basis 0) as the safe model, %q at basis %v is refused "+
		"as \"exceeds cost cap\". The zero blocks here and waves through in the test above.",
		safe.Model, rec.Model, recCost)
}
