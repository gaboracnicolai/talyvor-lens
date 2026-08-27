package proxy

import (
	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/catalog"
)

// budget_gate_estimate.go — the pre-serve BUDGET gate's cost estimate.
//
// ⚠ WHY THIS IS A NAMED FUNCTION AND NOT THREE INLINE WORDS. It mirrors
// lxcEstimate (lxc_gate.go) deliberately: the two are the same job — a pre-serve,
// input-only estimate that decides whether to refuse a request — and they sat
// twenty lines apart pricing the same request by different rules.
//
//	lxcEstimate      alerts.CostUSDResolved(model, catalog.PurposeCharge, …)
//	the budget gate  alerts.CostUSD(model, len(prompt)/4, 0)      ← this one
//
// alerts.CostUSD returns EXACTLY ZERO for any model the catalog does not hold, and
// the catalog holds 45: `gpt-4`, `gpt-4-turbo` and `claude-3-opus` are not among
// them (W6.16, #484). So the budget gate estimated every request on those models at
// $0 and decided on `spent + 0`.
//
// ⚠ THE PURPOSE IS THE WHOLE DECISION AND IT IS NOT MINE. catalog.PurposeHold falls
// back to the provider's MOST EXPENSIVE known model and PurposeCharge to its
// CHEAPEST, so Hold here would make this gate block more than it does today — a
// behaviour change on live traffic. This codebase has already made the choice
// explicitly and written down why: lxcEstimate, which is the pre-serve GATE, uses
// PurposeCharge, while reserveEstimateLXC, which is the RESERVATION, uses
// PurposeHold because "over-holding is refunded by the settle, under-holding leaks
// the ceiling". A gate is not a hold. PurposeCharge it is — which also preserves
// the budget gate's own documented intent, that it "under- rather than over-blocks".
//
// The fallback announces itself the same way lxcEstimate's does, so an operator can
// see which models are being gated on a guess.
func budgetEstimateUSD(model, prompt string) float64 {
	estUSD, prov := alerts.CostUSDResolved(model, catalog.PurposeCharge, len(prompt)/4, 0, 0, 0)
	if prov == catalog.ProvenanceFallback && len(prompt) > 0 {
		alerts.WarnUnpricedModel(model, catalog.PurposeCharge, estUSD)
	}
	return estUSD
}
