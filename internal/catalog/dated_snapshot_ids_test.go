package catalog

import "testing"

// ⚠ THE DATED SNAPSHOT ID IS THE PROVIDER'S *PRIMARY* ID, NOT AN OBSCURE VARIANT.
//
// Anthropic's model table lists "Claude API ID" as the DATED form (claude-haiku-4-5-20251001) and the
// bare name as a convenience *alias*. GET /v1/models returns the dated form. Anthropic's own docs
// recommend pinning a snapshot. So the id a careful client sends is precisely the one this catalog
// did not know — and ResolveRates does no date-suffix normalisation, so every such request billed on
// the derived floor while looking completely ordinary in the logs.
//
// The alias mechanism already existed and was already used for OpenAI's dated snapshots
// (gpt-4o-2024-11-20 and friends). It was simply never applied to Anthropic's.
//
//	Source: https://platform.claude.com/docs/en/about-claude/models/overview  (fetched 2026-07-28)
func TestDatedSnapshotIDsPriceExactly(t *testing.T) {
	// id → the bare alias it must price identically to.
	cases := map[string]string{
		"claude-haiku-4-5-20251001":  "claude-haiku-4-5",
		"claude-sonnet-4-5-20250929": "claude-sonnet-4-5",
		"claude-opus-4-5-20251101":   "claude-opus-4-5",
		"claude-opus-4-1-20250805":   "claude-opus-4-1",
	}
	for dated, bare := range cases {
		gotRates, gotProv := ResolveRates(dated, PurposeCharge)
		if gotProv != ProvenanceExact {
			wantRates, _ := ResolveRates(bare, PurposeCharge)
			t.Errorf("%s resolved %s, not exact — billed at $%.2f/$%.2f per 1M instead of $%.2f/$%.2f.\n"+
				"  This is the id the provider's docs call the PRIMARY \"Claude API ID\" and the id "+
				"GET /v1/models returns; a client pinning a snapshot, which the docs recommend, is "+
				"billed on the derived floor.",
				dated, gotProv, gotRates.InputPer1M, gotRates.OutputPer1M, wantRates.InputPer1M, wantRates.OutputPer1M)
			continue
		}
		wantRates, _ := ResolveRates(bare, PurposeCharge)
		if gotRates != wantRates {
			t.Errorf("%s and %s price differently (%+v vs %+v) — an alias must not drift from its target",
				dated, bare, gotRates, wantRates)
		}
	}
}

// The floor a fallback lands on is a coincidence, and coincidences move. claude-haiku-4-5-20251001
// happened to be billed correctly ONLY because Haiku 4.5 *is* the cheapest Anthropic model in the
// catalog; seeding anything cheaper would silently start under-billing it. Asserting exactness rather
// than the resulting number is what makes that irrelevant.
func TestFallbackFloorIsNotADefensiblePriceForAKnownModel(t *testing.T) {
	r, prov := ResolveRates("claude-haiku-4-5-20251001", PurposeCharge)
	if prov != ProvenanceExact {
		t.Fatalf("expected exact pricing, got %s at $%.2f/$%.2f", prov, r.InputPer1M, r.OutputPer1M)
	}
}
