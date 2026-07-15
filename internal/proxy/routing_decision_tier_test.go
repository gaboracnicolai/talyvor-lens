package proxy

import (
	"reflect"
	"testing"

	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/routing"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/internal/worktier"
)

// TestDecisionTier_DelegatesToWorkTierAdvisor — the Shape-1 consumer now derives
// its two gate signals from the FULL work-classification via the WorkTier Advisor:
// sensitive() == advice.SensitiveOptOut and smallSimple() == advice.DowngradeEligible
// for the SAME request signals. This is the "extend the consumer to the full
// work-classification" binding — the gate and the descriptive classifier speak one
// tier vocabulary, so they can never drift.
func TestDecisionTier_DelegatesToWorkTierAdvisor(t *testing.T) {
	a := worktier.NewAdvisor()
	for _, d := range []decisionTier{
		{inputTokens: 500, complexity: 1},
		{inputTokens: 999, complexity: 2},
		{inputTokens: 1000, complexity: 1},
		{inputTokens: 999, complexity: 3},
		{inputTokens: 500, complexity: 1, pii: true},
		{inputTokens: 500, complexity: 1, guardrail: true},
		{inputTokens: 500, complexity: 1, loggingNone: true},
		{inputTokens: 50000, complexity: 5},
	} {
		adv := a.Advise(d.signals())
		if d.sensitive() != adv.SensitiveOptOut {
			t.Errorf("sensitive()=%v but advisor SensitiveOptOut=%v for %+v", d.sensitive(), adv.SensitiveOptOut, d)
		}
		if d.smallSimple() != adv.DowngradeEligible {
			t.Errorf("smallSimple()=%v but advisor DowngradeEligible=%v for %+v", d.smallSimple(), adv.DowngradeEligible, d)
		}
	}
}

// qualifying recommendation + a premium base pick, the workhorses of the
// conservatism table. gpt-4o-mini (rank 1) is a strict downgrade from the
// premium base gpt-5.4 (rank 6) within the openai family.
func qualRec(model string) routing.Recommendation {
	return routing.Recommendation{Model: model, Provider: "openai", Basis: routing.BasisQualityPerDollar, Reason: "best q/$"}
}
func premiumBase() router.RoutingDecision {
	return router.RoutingDecision{Model: "gpt-5.4", Provider: "openai", Reason: "High complexity — premium model required"}
}

// TestDecisionTier_Sensitive — elevated (PII OR guardrail) and restricted
// (logging none) are sensitive; normal/metadata are not. Covers the OR.
func TestDecisionTier_Sensitive(t *testing.T) {
	for _, c := range []struct {
		name             string
		pii, guard, lnon bool
		want             bool
	}{
		{"normal", false, false, false, false},
		{"pii only", true, false, false, true},
		{"guardrail only", false, true, false, true}, // the OR
		{"both elevated", true, true, false, true},
		{"restricted", false, false, true, true},
		{"restricted+elevated", true, false, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := decisionTier{pii: c.pii, guardrail: c.guard, loggingNone: c.lnon}
			if got := d.sensitive(); got != c.want {
				t.Errorf("sensitive(pii=%v guard=%v loggingNone=%v) = %v, want %v", c.pii, c.guard, c.lnon, got, c.want)
			}
		})
	}
}

// TestDecisionTier_SmallSimple — eligible-for-downgrade iff small (input tokens)
// AND complexity ≤ simple. Pins both boundaries.
func TestDecisionTier_SmallSimple(t *testing.T) {
	for _, c := range []struct {
		name        string
		inTok, cplx int
		want        bool
	}{
		{"small+trivial", 999, 0, true},
		{"small+simple edge", 999, 2, true},
		{"small+moderate", 999, 3, false}, // complexity too high
		{"size edge not small", 1000, 1, false},
		{"large+simple", 50000, 1, false},
		{"large+complex", 50000, 5, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := decisionTier{inputTokens: c.inTok, complexity: c.cplx}
			if got := d.smallSimple(); got != c.want {
				t.Errorf("smallSimple(in=%d cplx=%d) = %v, want %v", c.inTok, c.cplx, got, c.want)
			}
		})
	}
}

// TestNewDecisionTier_DerivesFromSignals — the constructor derives complexity
// from the SAME prompt the router scores and maps logging=none → loggingNone,
// passing pii/guardrail through unchanged. No DB, no cross-tenant input.
func TestNewDecisionTier_DerivesFromSignals(t *testing.T) {
	// A code + multi-step prompt scores ≥ 2 on AnalyseComplexity.
	prompt := "```go\nfunc x(){}\n``` do this step by step, first...then finally"
	d := newDecisionTier(640, router.AnalyseComplexity(prompt).Score(), true, false, workspace.LoggingFull)
	if d.inputTokens != 640 || !d.pii || d.guardrail {
		t.Errorf("signals not passed through: %+v", d)
	}
	if d.complexity != router.AnalyseComplexity(prompt).Score() {
		t.Errorf("complexity must equal the router's score on the same prompt; got %d", d.complexity)
	}
	if d.loggingNone {
		t.Error("LoggingFull must not be loggingNone")
	}
	if r := newDecisionTier(10, router.AnalyseComplexity("hi").Score(), false, false, workspace.LoggingNone); !r.loggingNone {
		t.Error("LoggingNone must set loggingNone")
	}
}

// TestNewDecisionTier_TotalNoPanic — the gate is ON the serve path (pre-serve),
// so it must be total: adversarial inputs must never panic (a panic would break
// the served request). Best-effort = totality, not error-swallowing.
func TestNewDecisionTier_TotalNoPanic(t *testing.T) {
	for _, p := range []string{"", "x", string(make([]byte, 1<<20)), "🚀∑∫ proof", "```"} {
		_ = newDecisionTier(-5, router.AnalyseComplexity(p).Score(), false, false, workspace.LoggingMetadata) // negative tokens too
	}
}

// TestDecisionTier_NoStoreHandle_Structural — the decisionTier can NEVER be
// persisted: it holds only primitive request-local signals (no Store, pool, or
// interface handle). A future field that adds a handle trips this.
func TestDecisionTier_NoStoreHandle_Structural(t *testing.T) {
	tp := reflect.TypeOf(decisionTier{})
	for i := 0; i < tp.NumField(); i++ {
		switch k := tp.Field(i).Type.Kind(); k {
		case reflect.Int, reflect.Bool:
			// primitive request-local signal — OK
		default:
			t.Errorf("decisionTier.%s is %s — must hold only primitive signals (no Store/pool/interface; never persisted)",
				tp.Field(i).Name, tp.Field(i).Type)
		}
	}
}

// TestResolveAutoRoute_ConservatismInvariant — the headline proof. resolveAutoRoute
// is SUBTRACTIVE: its model output is ALWAYS rec.Model (accepted) or base.Model
// (the complexity router's pick) — never a third model. The Shape-1 gate can
// only SUPPRESS (sensitivity opt-out / downgrade veto), never select.
func TestResolveAutoRoute_ConservatismInvariant(t *testing.T) {
	rt := router.New()
	base := premiumBase()          // gpt-5.4 (rank 6)
	down := qualRec("gpt-4o-mini") // rank 1 — a strict downgrade vs base
	up := qualRec("gpt-5.4")       // == base, not a downgrade
	none := routing.Recommendation{Basis: routing.BasisNone}
	empty := routing.Recommendation{Model: "", Basis: routing.BasisQualityPerDollar}

	large := decisionTier{inputTokens: 50000, complexity: 0} // not small
	complex := decisionTier{inputTokens: 500, complexity: 5} // small but complex
	smallSimple := decisionTier{inputTokens: 500, complexity: 1}
	piiSmall := decisionTier{inputTokens: 500, complexity: 1, pii: true}
	guardSmall := decisionTier{inputTokens: 500, complexity: 1, guardrail: true}
	restricted := decisionTier{inputTokens: 500, complexity: 1, loggingNone: true}

	for _, c := range []struct {
		name      string
		rec       routing.Recommendation
		base      router.RoutingDecision
		dt        decisionTier
		wantModel string // expected upstream model
		wantApply bool
		wantGate  string
	}{
		// NULL / today-identical cases ((a) flag-off & (b) pinned model are
		// guarded upstream of this call and so cannot reach here).
		{"null/no-rec-basis-none", none, base, large, "gpt-5.4", false, ""},
		{"null/no-rec-empty-model", empty, base, large, "gpt-5.4", false, ""},
		{"null/rec-applied-small-simple-downgrade", down, base, smallSimple, "gpt-4o-mini", true, ""},
		{"null/rec-applied-non-downgrade", up, router.RoutingDecision{Model: "gpt-4o-mini", Provider: "openai"}, large, "gpt-5.4", true, ""},
		// VETO — downgrade on heavy work.
		{"veto/large+downgrade", down, base, large, "gpt-5.4", false, gateDowngradeVeto},
		{"veto/complex+downgrade", down, base, complex, "gpt-5.4", false, gateDowngradeVeto},
		// OPT-OUT — sensitivity beats the small-simple accept (precedence).
		{"optout/pii", down, base, piiSmall, "gpt-5.4", false, gateSensitivityOptOut},
		{"optout/guardrail", down, base, guardSmall, "gpt-5.4", false, gateSensitivityOptOut},
		{"optout/restricted", down, base, restricted, "gpt-5.4", false, gateSensitivityOptOut},
		// FAIL-OPEN — unknown rec model is not a known downgrade ⇒ applied (today's behavior).
		{"fail-open/unknown-rec-model", qualRec("mystery-model"), base, large, "mystery-model", true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := resolveAutoRoute(rt, c.rec, c.base, c.dt)
			if res.model != c.wantModel {
				t.Errorf("model = %q, want %q", res.model, c.wantModel)
			}
			if res.applied != c.wantApply {
				t.Errorf("applied = %v, want %v", res.applied, c.wantApply)
			}
			if res.gated != c.wantGate {
				t.Errorf("gated = %q, want %q", res.gated, c.wantGate)
			}
			// Outcome flags are mutually consistent: applied XOR fallback.
			if res.applied == res.fallback {
				t.Errorf("exactly one of applied/fallback must be set: applied=%v fallback=%v", res.applied, res.fallback)
			}
			if res.gated != "" && !res.fallback {
				t.Error("a gated result must take the fallback path")
			}
			// NEVER invents a third model.
			if res.model != "" && res.model != c.rec.Model && res.model != c.base.Model {
				t.Errorf("invented a third model %q (rec=%q base=%q)", res.model, c.rec.Model, c.base.Model)
			}
		})
	}
}

// TestResolveAutoRoute_OptOutFallsToConcreteBaseNotNil — a sensitive auto request
// with no pinned model must route to the base router's concrete pick, never to
// an empty model (which would forward "auto"/the requested model upstream).
func TestResolveAutoRoute_OptOutFallsToConcreteBaseNotNil(t *testing.T) {
	rt := router.New()
	base := router.RoutingDecision{Model: "gpt-4o", Provider: "openai", Reason: "moderate"}
	res := resolveAutoRoute(rt, qualRec("gpt-4o-mini"), base, decisionTier{pii: true})
	if res.gated != gateSensitivityOptOut {
		t.Fatalf("expected sensitivity opt-out, got %q", res.gated)
	}
	if res.model != "gpt-4o" {
		t.Errorf("opt-out must fall to the concrete base pick gpt-4o, got %q", res.model)
	}
}

// TestResolveAutoRoute_NeverInventsThirdModel — property sweep: across a grid of
// recs/bases/tiers, the output model is always rec.Model, base.Model, or "".
func TestResolveAutoRoute_NeverInventsThirdModel(t *testing.T) {
	rt := router.New()
	recs := []routing.Recommendation{
		qualRec("gpt-4o-mini"), qualRec("gpt-5.4"), qualRec("claude-haiku-4-6"),
		qualRec("mystery"), {Basis: routing.BasisNone},
	}
	bases := []router.RoutingDecision{
		{Model: "gpt-5.4", Provider: "openai"}, {Model: "gpt-4.1-nano", Provider: "openai"},
		{Model: "", Provider: "vllm"},
	}
	tiers := []decisionTier{
		{inputTokens: 500, complexity: 1}, {inputTokens: 50000, complexity: 5},
		{pii: true}, {guardrail: true}, {loggingNone: true},
	}
	for _, rc := range recs {
		for _, b := range bases {
			for _, dt := range tiers {
				res := resolveAutoRoute(rt, rc, b, dt)
				if res.model != "" && res.model != rc.Model && res.model != b.Model {
					t.Fatalf("third model %q invented (rec=%q base=%q dt=%+v)", res.model, rc.Model, b.Model, dt)
				}
			}
		}
	}
}

// TestRecDowngrades — strict same-provider cost reduction only; unknown /
// cross-provider / no-base ⇒ not a downgrade (fail-open).
func TestRecDowngrades(t *testing.T) {
	rt := router.New()
	prem := router.RoutingDecision{Model: "gpt-5.4", Provider: "openai"}
	cheap := router.RoutingDecision{Model: "gpt-4.1-nano", Provider: "openai"}
	for _, c := range []struct {
		name string
		rec  routing.Recommendation
		base router.RoutingDecision
		want bool
	}{
		{"cheaper same provider", qualRec("gpt-4o-mini"), prem, true},
		{"pricier same provider", qualRec("gpt-5.4"), cheap, false},
		{"equal", qualRec("gpt-5.4"), prem, false},
		{"cross provider", routing.Recommendation{Model: "claude-haiku-4-6", Provider: "anthropic"}, prem, false},
		{"unknown rec model", qualRec("mystery"), prem, false},
		{"no base model", qualRec("gpt-4o-mini"), router.RoutingDecision{Model: ""}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := recDowngrades(rt, c.rec, c.base); got != c.want {
				t.Errorf("recDowngrades = %v, want %v", got, c.want)
			}
		})
	}
}
