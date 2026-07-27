package catalog

import (
	"regexp"
	"testing"
)

// verifiedAnthropicModels is the pinned allow-list of Anthropic model IDs Lens is permitted to seed
// into the COST-ROUTING catalog.
//
// SOURCE: GET https://api.anthropic.com/v1/models, captured live on 2026-07-19 during the first
// production standup. The API returns some models undated (claude-opus-4-6, claude-sonnet-4-6,
// claude-opus-4-8, …) and some as dated snapshots (claude-haiku-4-5-20251001, …). Where the catalog
// and router use the undated alias of a dated snapshot, BOTH forms are listed below.
//
// INVARIANT: every catalog entry that is dispatched to Anthropic — Provider=="anthropic" directly, or
// Provider=="bedrock" whose id embeds an `anthropic.claude-<family>` model — MUST name a base model in
// this set.
//
// CATCHES: a phantom entry such as "claude-haiku-4-6" (there is NO Haiku 4.6 at any version). A
// catalog entry naming a nonexistent model is invisible until a live request is cost-routed to it and
// Anthropic returns 404 not_found_error — exactly the bug the first standup hit (a claude-sonnet-4-6
// request was downgraded to the phantom "cheapest" and 404'd).
//
// INVALIDATED BY: Anthropic releasing a new model (add it here, with a price taken from a citable
// source — never a guess, a wrong rate corrupts the cost/budget/forecast tables) or retiring one
// (remove it here AND from seed.go). This is a deliberately HUMAN-maintained mirror of /v1/models: a
// live network check in CI would be flaky and couple the build to an external API.
var verifiedAnthropicModels = map[string]struct{}{
	// ⚠ claude-opus-5 IS NOT FROM THE 2026-07-19 /v1/models CAPTURE. Added 2026-07-26 on different
	// evidence, stated plainly rather than folded into the list above:
	//   1. It is PUBLISHED on https://platform.claude.com/docs/en/about-claude/pricing with rates.
	//   2. Stronger — it is BEING SERVED THROUGH THIS PROXY TODAY. The unpriced-model hole this
	//      PR closes was found on live Opus 5 traffic, so its existence is not in question: requests
	//      on it succeeded and Talyvor paid the provider for them.
	// No API key was available in this environment to re-run GET /v1/models, so the capture itself was
	// NOT refreshed. If you have a key, re-capture and fold this entry into the list above.
	"claude-opus-5": {},
	// ⚠ claude-mythos-5 IS DELIBERATELY ABSENT, and so is its seed entry. Its PRICE is published
	// ($10/$50) but the page marks it "limited availability" and it is in neither the capture nor any
	// traffic I can point to. This guard is an EXISTENCE check, and widening it from a marketing page is
	// precisely the failure it was built to catch (there is no Haiku 4.6 either, and that phantom
	// 404'd a live request). If Mythos 5 is dispatchable, the fallback prices it and the detection job
	// added in this PR alerts on it — that is the safe order: alert first, then price.

	// returned undated by /v1/models
	"claude-sonnet-5":   {},
	"claude-fable-5":    {},
	"claude-opus-4-8":   {},
	"claude-opus-4-7":   {},
	"claude-sonnet-4-6": {},
	"claude-opus-4-6":   {},
	// dated snapshots + the undated alias each is served under (the forms the catalog/router use)
	"claude-opus-4-5-20251101":   {},
	"claude-opus-4-5":            {},
	"claude-haiku-4-5-20251001":  {},
	"claude-haiku-4-5":           {},
	"claude-sonnet-4-5-20250929": {},
	"claude-sonnet-4-5":          {},
	"claude-opus-4-1-20250805":   {},
	// Undated alias of the line above, added 2026-07-28 under this file's stated convention ("Where the
	// catalog and router use the undated alias of a dated snapshot, BOTH forms are listed"). No widening
	// of the existence claim: the 2026-07-19 capture already contains the dated form, and Anthropic's
	// model table gives claude-opus-4-1 as its alias. Deprecated; RETIRES 2026-08-05, at which point
	// this entry and its seed row should both be removed.
	"claude-opus-4-1": {},
}

// bedrockAnthropicRe extracts the base anthropic family from a Bedrock model id, e.g.
// "anthropic.claude-haiku-4-6-20251103-v1:0" -> "claude-haiku-4-6". Non-anthropic bedrock shapes
// don't match and are not covered by this invariant.
var bedrockAnthropicRe = regexp.MustCompile(`^anthropic\.(claude-[a-z]+-\d+-\d+)-\d{8}-v\d+:\d+$`)

// TestCatalog_NoPhantomAnthropicModel fails on the current catalog because of the phantom
// "claude-haiku-4-6" (and its Bedrock twin). It is the durable guard: a nonexistent model can never
// again be seeded into the cost-routing catalog without a RED here, offline, before any user 404s.
func TestCatalog_NoPhantomAnthropicModel(t *testing.T) {
	for _, m := range seedModels() {
		switch m.Provider {
		case "anthropic":
			if _, ok := verifiedAnthropicModels[m.ID]; !ok {
				t.Errorf("catalog seeds anthropic model %q — NOT in the verified /v1/models allow-list. "+
					"A phantom model is invisible until a cost-routed request 404s with not_found_error.", m.ID)
			}
		case "bedrock":
			mm := bedrockAnthropicRe.FindStringSubmatch(m.ID)
			if mm == nil {
				continue // not an anthropic-on-bedrock id shape
			}
			if _, ok := verifiedAnthropicModels[mm[1]]; !ok {
				t.Errorf("catalog seeds bedrock model %q (base %q) — base is NOT a verified anthropic model (phantom).", m.ID, mm[1])
			}
		}
	}
}
