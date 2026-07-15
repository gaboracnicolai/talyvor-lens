package worktier

import "fmt"

// This file adds the WorkTier ADVISOR — the read-only advisory surface over the
// descriptive classifier. Given a request's pre-serve, NON-CONTENT signals it (1)
// PROJECTS the full pre-serve work-classification (the three axes knowable before
// the serve) and (2) ADVISES a routing HINT derived from that tier. It is advice,
// NEVER currency: it lives in this DESCRIPTIVE, import-guarded package (no minter
// import — see TestWorkTier_MintFree_ImportGuard) and is STATELESS — it holds no
// DB/ledger handle at all (see TestAdvisor_MintFree_NoHandle_Structural), so no
// code path here can write a ledger or mint. The hint only INFORMS routing; the
// Shape-1 gate consumes it SUBTRACTIVELY (it can make routing more conservative,
// never select or upgrade a model).
//
// Why a DISTINCT pre-serve type (not the persisted WorkTier): before the serve,
//   - SIZE is INPUT tokens only — output is unknowable until the model responds;
//   - COST is ABSENT — it depends on the very model we are about to choose
//     (circular); the persisted WorkTier carries cost POST-serve, offline-only.
// So PreServeTier deliberately carries no Cost axis and its Size means "input
// magnitude". Complexity/Sensitivity share the SAME bucketers as Classify, so the
// pre-serve and post-serve tiers agree on those axes for the same raw signal.

// PreServeSignals are the request-local, NON-CONTENT signals knowable BEFORE the
// serve. Pure values only — no handles, nothing that could reach a ledger.
type PreServeSignals struct {
	InputTokens     int    // input magnitude (output is unknown pre-serve)
	ComplexityScore int    // router.AnalyseComplexity(prompt).Score() ∈ [0,5]
	PIIDetected     bool   // sensitivity cause A
	GuardrailFired  bool   // sensitivity cause B
	LoggingPolicy   string // "none" | "metadata" | "full"
}

// PreServeTier is the FULL pre-serve work-classification: the three axes knowable
// before the serve, in the SAME frozen bucket vocabulary as WorkTier. COST is
// intentionally omitted (unknowable pre-serve). Never persisted — a pure value.
type PreServeTier struct {
	Size        SizeBucket  `json:"size"` // from INPUT tokens only
	Complexity  Complexity  `json:"complexity"`
	Sensitivity Sensitivity `json:"sensitivity"`
}

// TierAdvice is the read-only routing advice for one request: the projected tier
// plus the two SUBTRACTIVE hints the Shape-1 gate consumes and a human rationale.
// Both booleans can only make routing MORE conservative; neither selects a model.
type TierAdvice struct {
	Tier              PreServeTier `json:"tier"`
	DowngradeEligible bool         `json:"downgrade_eligible"` // small (input) AND ≤ simple complexity
	SensitiveOptOut   bool         `json:"sensitive_optout"`   // any non-normal sensitivity
	Rationale         string       `json:"rationale"`
}

// Advisor is the stateless WorkTier advisory surface. Zero value is usable; the
// empty struct guarantees it can hold no persistence handle (structurally
// mint-free). Safe for concurrent use.
type Advisor struct{}

// NewAdvisor returns the advisory surface.
func NewAdvisor() *Advisor { return &Advisor{} }

// Project derives the full pre-serve tier from request-local signals. PURE, total
// (no panic on adversarial input), mint-free. Reuses the SAME bucketers as the
// post-serve Classify (size on INPUT tokens here, since output is unknown).
func (a *Advisor) Project(sig PreServeSignals) PreServeTier {
	return PreServeTier{
		Size:        sizeBucket(sig.InputTokens),
		Complexity:  complexityBucket(sig.ComplexityScore),
		Sensitivity: sensitivityFor(sig.PIIDetected, sig.GuardrailFired, sig.LoggingPolicy),
	}
}

// Advise projects the tier and recommends the routing hint. DowngradeEligible is
// the ONLY shape eligible to accept a downgrade recommendation (small INPUT AND at
// most "simple" complexity); SensitiveOptOut fires for any non-normal sensitivity
// (a request that must leave the pooled routing path). Read-only, mint-free.
func (a *Advisor) Advise(sig PreServeSignals) TierAdvice {
	t := a.Project(sig)
	adv := TierAdvice{
		Tier:              t,
		DowngradeEligible: t.downgradeEligible(),
		SensitiveOptOut:   t.Sensitivity != SensitivityNormal,
	}
	adv.Rationale = adv.rationale()
	return adv
}

// downgradeEligible reports small (by INPUT tokens) AND at most "simple"
// complexity — the sole shape that may accept a downgrade. Mirrors the Shape-1
// decisionTier.smallSimple() contract, now sourced from the full tier vocabulary.
func (t PreServeTier) downgradeEligible() bool {
	return t.Size == SizeSmall && (t.Complexity == ComplexityTrivial || t.Complexity == ComplexitySimple)
}

// rationale renders a short human explanation of the advice (dashboard/introspection).
func (adv TierAdvice) rationale() string {
	var hint string
	switch {
	case adv.SensitiveOptOut:
		hint = "opt out of pooled routing intelligence (sensitivity " + string(adv.Tier.Sensitivity) + ")"
	case adv.DowngradeEligible:
		hint = "eligible for a downgrade recommendation (small + low complexity)"
	default:
		hint = "hold the routing baseline (not small-simple)"
	}
	return fmt.Sprintf("size=%s complexity=%s sensitivity=%s ⇒ %s",
		adv.Tier.Size, adv.Tier.Complexity, adv.Tier.Sensitivity, hint)
}
