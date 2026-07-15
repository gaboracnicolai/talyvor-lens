package proxy

import (
	"github.com/talyvor/lens/internal/modelcapability"
	"github.com/talyvor/lens/internal/routingbrain"
	"github.com/talyvor/lens/internal/worktier"
)

// SetRoutingBrain wires the serving-side Routing Brain (H8.1). A nil / disabled
// brain leaves routing byte-for-byte unchanged (the apply path early-returns).
func (p *Proxy) SetRoutingBrain(b *routingbrain.Brain) { p.routingBrain = b }

// brainDifficulty derives the H1/H2 work-tier difficulty for a request PRE-SERVE —
// H1's pre-serve projection (size on INPUT tokens + complexity) → H2's difficulty
// ordinal. This is the cohort key the offline job used, so the serve-path lookup
// hits the right recommendation. Pure; sensitivity/cost are irrelevant to difficulty.
func brainDifficulty(inputTokens, complexityScore int) int {
	tier := worktier.NewAdvisor().Project(worktier.PreServeSignals{
		InputTokens: inputTokens, ComplexityScore: complexityScore,
	})
	return modelcapability.DifficultyOf(tier)
}

// applyRoutingBrain resolves the brain's advisory/autonomous decision for an
// AUTO-ROUTE request. It is a cheap in-memory lookup (no computation, no DB on the
// serve path). Returns the (possibly overridden) model, a reason, whether the brain
// APPLIED its pick (autonomous + hard floor passed), and whether a recommendation
// was surfaced at all.
//
//   - brain nil/off, or no recommendation for the cohort → (safeModel, "", false, false):
//     routing is untouched.
//   - ADVISORY (or an autonomous floor fallback) → (safeModel, reason, false, true):
//     the recommendation is surfaced but the route stays the router's.
//   - AUTONOMOUS + floor ok → (brainModel, reason, true, true): the brain's pick is the route.
func (p *Proxy) applyRoutingBrain(workspaceID string, inputTokens, complexityScore int, safeModel string, allowedModels []string) (model, reason string, applied, surfaced bool) {
	if p == nil || !p.routingBrain.Enabled() || safeModel == "" {
		return safeModel, "", false, false
	}
	dec, ok := p.routingBrain.Resolve(workspaceID, brainDifficulty(inputTokens, complexityScore), safeModel, allowedModels)
	if !ok {
		return safeModel, "", false, false
	}
	return dec.Model, dec.Reason, dec.Applied, true
}
