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
