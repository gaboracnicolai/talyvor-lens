package proxy

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
)

// budget_gate_estimate_test.go — the budget gate's pre-serve estimate, against the
// LXC gate's, for the same request.
//
// The two are the same job twenty lines apart in the same request path: an
// input-only pre-serve estimate that decides whether to refuse. They must not price
// one request two ways, and in particular the budget gate must not price a whole
// class of models at exactly zero — which is what alerts.CostUSD does for anything
// the 45-model catalog does not hold.

// budgetProbePrompt is long enough that len/4 is a real token count, so a zero
// estimate cannot be blamed on an empty prompt.
var budgetProbePrompt = strings.Repeat("a", 40_000)

func TestBudgetEstimate_UnpricedModelIsNotZero(t *testing.T) {
	// PREMISE, asserted rather than assumed — if gpt-4 gets priced, this stops
	// measuring what it claims to and says so.
	if _, _, ok := catalog.Price(unpricedProbeModel); ok {
		t.Fatalf("%s is now in the catalog; pick another absent model or delete this test with "+
			"the reason", unpricedProbeModel)
	}

	got := budgetEstimateUSD(unpricedProbeModel, budgetProbePrompt)
	if got <= 0 {
		t.Errorf("the budget gate estimated %v for a 10k-token prompt on %q.\n"+
			"    budgets.CheckBudget decides on `spent + estCost`, so a structurally-zero estimate "+
			"means a request that would cross a hard_block limit is never seen to cross it.\n"+
			"    The LXC gate, twenty lines below and doing the same pre-serve job, prices the "+
			"same request through alerts.CostUSDResolved.", got, unpricedProbeModel)
	}
}

// ⚠ THE MIRROR THIS ITEM CLAIMS, ASSERTED AGAINST lxcEstimate ITSELF.
//
// ⚠ ITS FIRST DRAFT COMPARED AGAINST A HAND-TYPED COPY of lxcEstimate's formula —
// alerts.CostUSDResolved(model, PurposeCharge, len/4, 0,0,0) written out again here —
// and control L6 caught it: change lxcEstimate's purpose and this test agreed with
// the new answer instead of noticing. A transcription of the thing you are comparing
// against is a SECOND SOURCE OF TRUTH, and it drifts silently. It now calls the real
// function and asserts the exact identity between them: lxcEstimate is
// ceil(usd / LXCUSDValue * 1e6) over the same basis, so if the two bases agree this
// holds exactly, and if lxcEstimate's basis moves it cannot.
func TestBudgetEstimate_MatchesTheLXCGatesBasis(t *testing.T) {
	for _, model := range []string{unpricedProbeModel, pricedProbeModel} {
		budget := budgetEstimateUSD(model, budgetProbePrompt)
		if budget <= 0 {
			t.Errorf("%s: the budget gate prices this request at %v, so the identity below is "+
				"satisfied by two zeros and proves nothing", model, budget)
			continue
		}
		wantMicroLXC := int64(math.Ceil(budget / economy.LXCUSDValue * 1e6))
		if got := lxcEstimate(model, budgetProbePrompt); got != wantMicroLXC {
			t.Errorf("%s: lxcEstimate = %d µLXC, but the budget gate's basis (%v USD) implies %d. "+
				"The two pre-serve gates are pricing the same request differently — one of them "+
				"is refusing traffic the other would allow.", model, got, budget, wantMicroLXC)
		}
	}
}

// ⚠ THE CONTROL THAT MATTERS: a PRICED model must estimate exactly what it
// estimated before. CostUSDResolved returns the exact catalog price when there is
// one, so this change must be invisible to the 45 models the catalog holds. If it
// is not, the mirror was not exact and the item is wrong.
func TestBudgetEstimate_PricedModelIsUnchangedFromTheOldHelper(t *testing.T) {
	want := alerts.CostUSD(pricedProbeModel, len(budgetProbePrompt)/4, 0)
	if want <= 0 {
		t.Fatalf("the priced control estimates %v — it is not a control", want)
	}
	if got := budgetEstimateUSD(pricedProbeModel, budgetProbePrompt); got != want {
		t.Errorf("a PRICED model now estimates %v, want %v — exactly what alerts.CostUSD produced "+
			"before. Only unpriced models were supposed to move.", got, want)
	}
}

// The purpose argument decides the direction of the fallback, and picking the wrong
// one turns an under-blocking gate into an over-blocking one. PurposeHold falls back
// to the provider's most expensive known model; a GATE must not.
func TestBudgetEstimate_UsesTheChargeFallbackNotTheHoldFallback(t *testing.T) {
	charge, _ := alerts.CostUSDResolved(unpricedProbeModel, catalog.PurposeCharge,
		len(budgetProbePrompt)/4, 0, 0, 0)
	hold, _ := alerts.CostUSDResolved(unpricedProbeModel, catalog.PurposeHold,
		len(budgetProbePrompt)/4, 0, 0, 0)
	if charge == hold {
		t.Skip("charge and hold fall back to the same rate for this model — the distinction this " +
			"test guards is not observable here")
	}
	got := budgetEstimateUSD(unpricedProbeModel, budgetProbePrompt)
	if got == hold {
		t.Errorf("the budget gate estimates %v, which is the HOLD fallback (the provider's most "+
			"expensive known model). A pre-serve GATE uses the CHARGE fallback (%v) — lxcEstimate "+
			"does, and the gate's own comment says it under- rather than over-blocks.", got, charge)
	}
	if got != charge {
		t.Errorf("the budget gate estimates %v, want the charge fallback %v", got, charge)
	}
}

func TestBudgetEstimate_EmptyPromptIsStillZero(t *testing.T) {
	// Unchanged behaviour, pinned: no prompt means no input tokens means no
	// estimate, and lxcEstimate suppresses its unpriced warning on exactly this
	// case for the same reason.
	if got := budgetEstimateUSD(pricedProbeModel, ""); got != 0 {
		t.Errorf("empty prompt estimated %v, want 0", got)
	}
}

// ── the wiring ──

func TestBudgetGateUsesTheNamedEstimate(t *testing.T) {
	raw, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "estCost := budgetEstimateUSD(model, prompt)") {
		t.Error("the budget gate no longer calls budgetEstimateUSD; every test in this file " +
			"drives a function the gate does not use")
	}
	if strings.Contains(src, "estCost := alerts.CostUSD(") {
		t.Error("the budget gate is back on alerts.CostUSD, which is exactly zero for any model " +
			"the catalog does not hold")
	}
}
