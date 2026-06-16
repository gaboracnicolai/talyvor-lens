package proxy

import (
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/routing"
	"github.com/talyvor/lens/internal/workspace"
)

// decisionTier is the PRE-SERVE, request-local work tier used ONLY to gate
// acceptance of a U22 routing recommendation (Shape 1, issue #198). It is a
// DISTINCT type from worktier.WorkTier (the post-serve, persisted descriptive
// classification) and deliberately shares NONE of its identity:
//
//   - SIZE here is INPUT tokens only — output is unknowable before the serve.
//     The recorded WorkTier sizes on input+output TOTAL. Same numeric boundary,
//     different meaning ⇒ deliberately different concepts (do not unify them).
//   - COST is intentionally ABSENT. Pre-serve cost is unknowable: it depends on
//     the very model we are about to choose (circular). The recorded cost bucket
//     is OFFLINE-CALIBRATION-ONLY and must never gate the live decision.
//   - It is NEVER persisted. This type holds no Store handle and no Record path
//     (enforced structurally — all fields are primitive request-local signals);
//     it is built at the routing decision point, consulted, and discarded. The
//     post-serve captureWorkTier path is wholly untouched.
//
// Complexity is the SAME router.AnalyseComplexity(compressedPrompt) score the
// router and the post-serve capture use, so the gate's view of complexity equals
// the routing decision's by construction.
type decisionTier struct {
	inputTokens int
	complexity  int // router.AnalyseComplexity(compressedPrompt).Score() ∈ [0,5]
	pii         bool
	guardrail   bool
	loggingNone bool
}

// Boundaries mirror worktier's small/simple bands on the SAME signals but are
// declared locally so the routing gate carries no dependency on — and cannot be
// confused with — the persisted classifier's identity (see the type comment).
const (
	decisionSizeSmallMaxTokens  = 1000 // input tokens < this ⇒ "small"
	decisionComplexitySimpleMax = 2    // score ≤ this ⇒ "simple" (trivial 0, simple 1–2)
)

// newDecisionTier builds the pre-serve tier from request-local signals ONLY —
// no DB read, no cross-tenant data. Total (no error, no panic) so it can never
// break the served response.
func newDecisionTier(inputTokens int, compressedPrompt string, piiDetected, guardrailFired bool, loggingPolicy workspace.LoggingPolicy) decisionTier {
	return decisionTier{
		inputTokens: inputTokens,
		complexity:  router.AnalyseComplexity(compressedPrompt).Score(),
		pii:         piiDetected,
		guardrail:   guardrailFired,
		loggingNone: loggingPolicy == workspace.LoggingNone,
	}
}

// sensitive reports elevated (PII or a fired guardrail) or restricted (logging
// none) sensitivity. Such requests OPT OUT of cross-tenant routing intelligence
// entirely — they must leave the pooled path. Privacy-positive.
func (d decisionTier) sensitive() bool { return d.pii || d.guardrail || d.loggingNone }

// smallSimple reports small (by INPUT tokens) AND at most "simple" complexity —
// the only shape eligible to accept a downgrade recommendation.
func (d decisionTier) smallSimple() bool {
	return d.inputTokens < decisionSizeSmallMaxTokens && d.complexity <= decisionComplexitySimpleMax
}

// Shape-1 gate outcomes (also the RoutingTierGated metric reason labels).
const (
	gateSensitivityOptOut = "sensitivity_optout"
	gateDowngradeVeto     = "downgrade_veto"
)

// autoRouteResult is the resolved upstream-model decision for an auto-route
// request. model=="" means "leave the requested model unchanged" (no concrete
// base pick — e.g. vLLM passthrough or no router). Exactly one of applied/
// fallback is true; gated is set only alongside fallback.
type autoRouteResult struct {
	model    string
	reason   string
	applied  bool   // RoutingIntelligenceApplied — rec accepted
	gated    string // "" or a gate* reason — RoutingTierGated(reason)
	fallback bool   // RoutingFallback — base path taken
}

// resolveAutoRoute decides the upstream model for an auto-route request. It is
// SUBTRACTIVE: the result model is ALWAYS either rec.Model (recommendation
// accepted) or base.Model (the complexity router's pick) — never a third model,
// never an upgrade Shape 1 invents. The Shape-1 gate can only SUPPRESS a
// qualifying recommendation (sensitivity opt-out or downgrade veto), never
// select one.
//
// base is the model the request would use if NO recommendation were applied
// (the complexity router's pick); it is BOTH the downgrade baseline AND the
// model used on suppression. dt is the request-local pre-serve tier. Pure given
// (rt, rec, base, dt) — no DB, no cross-tenant read; the Advisor that produced
// rec stays workspace-blind. The gate is fail-open: any uncertainty (unknown
// model rank, unset signal) yields apply-rec — today's behavior — never a
// spurious veto, so it cannot break or distort the served request.
func resolveAutoRoute(rt *router.Router, rec routing.Recommendation, base router.RoutingDecision, dt decisionTier) autoRouteResult {
	applyRec := rec.Basis != routing.BasisNone && rec.Model != ""
	gateReason := ""
	if applyRec {
		switch {
		case dt.sensitive():
			gateReason = gateSensitivityOptOut // sensitivity wins over a small-simple accept
		case recDowngrades(rt, rec, base) && !dt.smallSimple():
			gateReason = gateDowngradeVeto
		}
		if gateReason != "" {
			applyRec = false
		}
	}
	if applyRec {
		return autoRouteResult{model: rec.Model, reason: rec.Reason, applied: true}
	}
	// Suppressed — no qualifying rec, or the Shape-1 gate vetoed/opted-out. An
	// auto request still gets a concrete model from the base router.
	res := autoRouteResult{gated: gateReason, fallback: true}
	if base.Model != "" {
		res.model = base.Model
		if gateReason != "" {
			res.reason = "routing tier-gated (" + gateReason + "): " + base.Reason
		} else {
			res.reason = "routing default (no qualifying intelligence): " + base.Reason
		}
	}
	return res
}

// recDowngrades reports whether applying rec.Model would be a DOWNGRADE versus
// the base-path model — a strictly cheaper model in the same provider family.
// Reuses the router's rank comparison (ShouldOverride), so "cheaper" means
// exactly what the pinned routing path means. Unknown model / cross-provider /
// no base ⇒ NOT a downgrade (the gate is fail-open toward today's behavior and
// never fabricates a veto).
func recDowngrades(rt *router.Router, rec routing.Recommendation, base router.RoutingDecision) bool {
	if rt == nil || base.Model == "" {
		return false
	}
	return rt.ShouldOverride(base.Model, router.RoutingDecision{Model: rec.Model, Provider: rec.Provider})
}
