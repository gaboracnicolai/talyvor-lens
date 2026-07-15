package worktier

import (
	"reflect"
	"strings"
	"testing"
)

// TestAdvisor_Project_FullPreServeClassification — Project derives the FULL
// pre-serve work-classification (the three axes knowable before the serve) in
// the SAME frozen bucket vocabulary as Classify: SIZE from INPUT tokens only
// (output is unknown pre-serve), COMPLEXITY from the router score, SENSITIVITY
// from pii/guardrail/logging. COST is deliberately absent pre-serve (it depends
// on the model about to be chosen — circular), so PreServeTier carries no Cost
// axis at all (compile-enforced).
func TestAdvisor_Project_FullPreServeClassification(t *testing.T) {
	a := NewAdvisor()
	// Size uses INPUT tokens ONLY (contrast Classify, which sums input+output).
	for _, c := range []struct {
		in   int
		want SizeBucket
	}{{0, SizeSmall}, {999, SizeSmall}, {1000, SizeMedium}, {9999, SizeMedium},
		{10000, SizeLarge}, {99999, SizeLarge}, {100000, SizeXLarge}} {
		if got := a.Project(PreServeSignals{InputTokens: c.in}).Size; got != c.want {
			t.Errorf("Project size(input=%d) = %q, want %q", c.in, got, c.want)
		}
	}
	// Complexity edges — same mapping as Classify/ComplexityBucketFor.
	for _, c := range []struct {
		score int
		want  Complexity
	}{{0, ComplexityTrivial}, {2, ComplexitySimple}, {3, ComplexityModerate}, {5, ComplexityComplex}} {
		if got := a.Project(PreServeSignals{ComplexityScore: c.score}).Complexity; got != c.want {
			t.Errorf("Project complexity(%d) = %q, want %q", c.score, got, c.want)
		}
	}
	// Sensitivity precedence: restricted (logging none) > elevated (pii OR guardrail) > normal.
	for _, c := range []struct {
		pii, guard bool
		policy     string
		want       Sensitivity
	}{
		{false, false, "full", SensitivityNormal},
		{true, false, "full", SensitivityElevated},
		{false, true, "full", SensitivityElevated},
		{false, false, "none", SensitivityRestricted},
		{true, true, "none", SensitivityRestricted}, // logging precedence over elevated
	} {
		if got := a.Project(PreServeSignals{PIIDetected: c.pii, GuardrailFired: c.guard, LoggingPolicy: c.policy}).Sensitivity; got != c.want {
			t.Errorf("Project sensitivity(pii=%v guard=%v policy=%q) = %q, want %q", c.pii, c.guard, c.policy, got, c.want)
		}
	}
}

// TestAdvisor_Project_SharesBucketersWithClassify — the pre-serve projection uses
// the SAME complexity/sensitivity bucketers as the persisted post-serve Classify,
// so a request's pre-serve tier and its post-serve tier agree on those axes on the
// same raw signal (size differs only by the in/out split, unknown pre-serve). This
// is what lets H2 bind one difficulty vocabulary across both.
func TestAdvisor_Project_SharesBucketersWithClassify(t *testing.T) {
	a := NewAdvisor()
	sig := PreServeSignals{InputTokens: 640, ComplexityScore: 4, PIIDetected: true, LoggingPolicy: "metadata"}
	pre := a.Project(sig)
	// Post-serve classification of the SAME request (output=0 here, cost irrelevant to these axes).
	post := Classify(sig.InputTokens, 0, 0, sig.ComplexityScore, sig.PIIDetected, sig.GuardrailFired, sig.LoggingPolicy)
	if pre.Complexity != post.Complexity {
		t.Errorf("pre-serve complexity %q != post-serve %q (must share bucketer)", pre.Complexity, post.Complexity)
	}
	if pre.Sensitivity != post.Sensitivity {
		t.Errorf("pre-serve sensitivity %q != post-serve %q (must share bucketer)", pre.Sensitivity, post.Sensitivity)
	}
	if pre.Size != post.Size { // equal here because output is 0; documents the input-only rule
		t.Errorf("pre-serve size %q != post-serve %q for output=0", pre.Size, post.Size)
	}
}

// TestAdvisor_PreServeSizeIsInputOnly_DivergesFromPostServe — the deliberate
// divergence: pre-serve SIZE is INPUT tokens only (output is unknown before the
// serve), so a 900-input request whose 100-token output tips the TOTAL to 1000 is
// pre-serve "small" yet post-serve "medium". The two size axes must NOT be unified.
func TestAdvisor_PreServeSizeIsInputOnly_DivergesFromPostServe(t *testing.T) {
	a := NewAdvisor()
	pre := a.Project(PreServeSignals{InputTokens: 900})
	post := Classify(900, 100, 0, 0, false, false, "full") // total = 1000
	if pre.Size != SizeSmall {
		t.Errorf("pre-serve size(input=900) = %q, want small (input-only)", pre.Size)
	}
	if post.Size != SizeMedium {
		t.Errorf("post-serve size(total=1000) = %q, want medium (total)", post.Size)
	}
}

// TestAdvisor_Advise_ReproducesShape1Semantics — the ROUTING HINT is the exact
// contract the Shape-1 gate consumes: DowngradeEligible == small(INPUT) AND ≤simple
// complexity; SensitiveOptOut == any non-normal sensitivity. This grid is the
// behavior-preservation pin: the existing decisionTier.smallSimple()/sensitive()
// gate must delegate to these without changing a single routing outcome.
func TestAdvisor_Advise_ReproducesShape1Semantics(t *testing.T) {
	a := NewAdvisor()
	for _, c := range []struct {
		name          string
		in, cplx      int
		pii, gd, lnon bool
		wantDowngrade bool
		wantOptOut    bool
	}{
		{"small+trivial", 999, 0, false, false, false, true, false},
		{"small+simple edge", 999, 2, false, false, false, true, false},
		{"small+moderate", 999, 3, false, false, false, false, false}, // complexity too high
		{"size edge not small", 1000, 1, false, false, false, false, false},
		{"large+simple", 50000, 1, false, false, false, false, false},
		{"large+complex", 50000, 5, false, false, false, false, false},
		{"pii small-simple", 999, 1, true, false, false, true, true},   // downgrade-eligible BUT opts out
		{"guardrail small-simple", 999, 1, false, true, false, true, true},
		{"restricted small-simple", 999, 1, false, false, true, true, true},
		{"restricted large-complex", 50000, 5, false, false, true, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			adv := a.Advise(PreServeSignals{
				InputTokens: c.in, ComplexityScore: c.cplx,
				PIIDetected: c.pii, GuardrailFired: c.gd,
				LoggingPolicy: map[bool]string{true: "none", false: "full"}[c.lnon],
			})
			if adv.DowngradeEligible != c.wantDowngrade {
				t.Errorf("DowngradeEligible = %v, want %v", adv.DowngradeEligible, c.wantDowngrade)
			}
			if adv.SensitiveOptOut != c.wantOptOut {
				t.Errorf("SensitiveOptOut = %v, want %v", adv.SensitiveOptOut, c.wantOptOut)
			}
			// The advice always carries the full projected tier it reasoned from.
			if adv.Tier != a.Project(PreServeSignals{
				InputTokens: c.in, ComplexityScore: c.cplx,
				PIIDetected: c.pii, GuardrailFired: c.gd,
				LoggingPolicy: map[bool]string{true: "none", false: "full"}[c.lnon],
			}) {
				t.Errorf("advice tier %+v does not match Project", adv.Tier)
			}
			if adv.Rationale == "" {
				t.Error("advice must carry a human rationale")
			}
		})
	}
}

// TestAdvisor_Advise_RationaleReflectsTier — the rationale is descriptive advice a
// human/dashboard can read; it names the axes it reasoned from.
func TestAdvisor_Advise_RationaleReflectsTier(t *testing.T) {
	a := NewAdvisor()
	adv := a.Advise(PreServeSignals{InputTokens: 500, ComplexityScore: 1, LoggingPolicy: "full"})
	for _, want := range []string{string(adv.Tier.Size), string(adv.Tier.Complexity), string(adv.Tier.Sensitivity)} {
		if !strings.Contains(adv.Rationale, want) {
			t.Errorf("rationale %q should mention axis value %q", adv.Rationale, want)
		}
	}
}

// TestAdvisor_Total_NoPanic — the Advisor is consulted PRE-SERVE on the request
// path, so it must be total: adversarial inputs (negative tokens, huge, unknown
// policy) must never panic (a panic would break the served request).
func TestAdvisor_Total_NoPanic(t *testing.T) {
	a := NewAdvisor()
	for _, sig := range []PreServeSignals{
		{InputTokens: -5, ComplexityScore: -1},
		{InputTokens: 1 << 30, ComplexityScore: 99},
		{LoggingPolicy: "??? unknown"},
		{},
	} {
		_ = a.Project(sig)
		_ = a.Advise(sig)
	}
}

// TestAdvisor_MintFree_NoHandle_Structural — the Advisor is a READ-ONLY advisory
// surface: it is stateless and holds NO field that could carry a DB/ledger/mint
// handle (no pointer, interface, func, or map). A future edit that gives it a
// persistence handle trips this — the structural half of "advisory only, never
// mints" (the import guard in worktier_test.go is the other half).
func TestAdvisor_MintFree_NoHandle_Structural(t *testing.T) {
	tp := reflect.TypeOf(Advisor{})
	for i := 0; i < tp.NumField(); i++ {
		switch k := tp.Field(i).Type.Kind(); k {
		case reflect.Bool, reflect.Int, reflect.Int64, reflect.Float64, reflect.String:
			// pure config/threshold value — cannot reach a ledger
		default:
			t.Errorf("Advisor.%s is %s — the advisor must hold no DB/ledger handle (no pointer/interface/func/map)",
				tp.Field(i).Name, tp.Field(i).Type)
		}
	}
}
