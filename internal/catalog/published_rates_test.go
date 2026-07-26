package catalog

import (
	"math"
	"testing"
)

// nearly compares money-per-1M rates with a tolerance, because rates withCacheRates DERIVES (input x a
// provider multiplier) land on binary-float artefacts: 0.10 * 3.00 == 0.30000000000000004. A
// 1e-9 tolerance is far below a millionth of a cent per 1M tokens and cannot mask a real mispricing.
func nearly(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// EVERY RATE HERE IS PINNED TO ITS PUBLISHED SOURCE, with the URL and fetch date.
//
// A wrong number in the catalog is worse than no number: the unknown-model fallback is honest about
// being a derived floor, whereas a wrong priced row is silently authoritative and bills real money.
// So each expectation below was transcribed from the provider's own pricing page — not inferred from a
// sibling model, not interpolated across a version series, not taken second-hand.
//
//	Anthropic: https://platform.claude.com/docs/en/about-claude/pricing   (fetched 2026-07-26)
//	OpenAI:    https://developers.openai.com/api/docs/pricing             (fetched 2026-07-26)
//
// ⚠ THIS TEST CANNOT DETECT A PRICE CHANGE. It pins the catalog to what was published on the fetch
// date; if a provider changes a rate tomorrow this test stays green while Lens bills the old one. That
// gap is real and is stated in model_detection.go — the only closure is a periodic human re-read of
// these two pages.
// publishedRateCase is one model's published rates plus the citation they came from. Exported to the
// package's tests (not just this file) so the pre-consolidation parity table can be cross-checked
// against it — see TestParityTableAgreesWithPublishedRates.
type publishedRateCase struct {
	id                       string
	in, cachedIn, write, out float64
	source                   string
}

func publishedRateCases() []publishedRateCase {
	return []publishedRateCase{
		// ── Anthropic ────────────────────────────────────────────────────────────────────
		{"claude-opus-5", 5.00, 0.50, 6.25, 25.00, "anthropic pricing: Claude Opus 5 — $5 / 5m write $6.25 / hit $0.50 / $25"},
		{"claude-opus-4-8", 5.00, 0.50, 6.25, 25.00, "anthropic pricing: Claude Opus 4.8"},
		{"claude-opus-4-7", 5.00, 0.50, 6.25, 25.00, "anthropic pricing: Claude Opus 4.7"},
		// CORRECTED: the catalog carried 15.00/75.00 — Opus 4.1's rate — and over-billed these 3x.
		{"claude-opus-4-6", 5.00, 0.50, 6.25, 25.00, "anthropic pricing: Claude Opus 4.6 (was wrongly 15/75)"},
		{"claude-opus-4-5", 5.00, 0.50, 6.25, 25.00, "anthropic pricing: Claude Opus 4.5 (was wrongly 15/75)"},
		// Sonnet 5 INTRODUCTORY rate, in force through 2026-08-31. Becomes 3.00/0.30/3.75/15.00 on 09-01.
		{"claude-sonnet-5", 2.00, 0.20, 2.50, 10.00, "anthropic pricing: Claude Sonnet 5 through August 31, 2026"},
		{"claude-sonnet-4-6", 3.00, 0.30, 3.75, 15.00, "anthropic pricing: Claude Sonnet 4.6"},
		{"claude-sonnet-4-5", 3.00, 0.30, 3.75, 15.00, "anthropic pricing: Claude Sonnet 4.5"},
		// CORRECTED: the catalog carried 0.80/4.00 — Haiku 3.5's rate — and under-billed Haiku 4.5 by 20%.
		{"claude-haiku-4-5", 1.00, 0.10, 1.25, 5.00, "anthropic pricing: Claude Haiku 4.5 (was wrongly 0.80/4.00)"},
		{"claude-fable-5", 10.00, 1.00, 12.50, 50.00, "anthropic pricing: Claude Fable 5"},

		// ── OpenAI (no cache-write charge published for these; write == 0 ⇒ derived, see withCacheRates)
		{"gpt-5.6-sol", 5.00, 0.50, 0, 30.00, "openai pricing: gpt-5.6-sol"},
		{"gpt-5.6-terra", 2.50, 0.25, 0, 15.00, "openai pricing: gpt-5.6-terra"},
		{"gpt-5.6-luna", 1.00, 0.10, 0, 6.00, "openai pricing: gpt-5.6-luna"},
		{"gpt-5.5", 5.00, 0.50, 0, 30.00, "openai pricing: gpt-5.5"},
		{"gpt-5.5-pro", 30.00, 30.00, 0, 180.00, "openai pricing: gpt-5.5-pro — Cached column is '—', so no discount"},
		{"gpt-5.4-pro", 30.00, 30.00, 0, 180.00, "openai pricing: gpt-5.4-pro — Cached column is '—'"},
		// CORRECTED: the catalog carried 5.00/20.00 and over-billed gpt-5.4 2x on input.
		{"gpt-5.4", 2.50, 0.25, 0, 15.00, "openai pricing: gpt-5.4 (was wrongly 5.00/20.00)"},
		// CORRECTED: the catalog carried 0.50/2.00 and under-billed gpt-5.4-mini.
		{"gpt-5.4-mini", 0.75, 0.075, 0, 4.50, "openai pricing: gpt-5.4-mini (was wrongly 0.50/2.00)"},

		// ── Amazon Bedrock ── AWS Marketplace "Bedrock Edition" listings (fetched 2026-07-26).
		// aws.amazon.com/bedrock/pricing does NOT list the 4.6 models; these are the AWS-owned
		// Marketplace product pages, which do. NO Bedrock premium exists — same rate as direct.
		// cachedIn/write are DERIVED by withCacheRates (anthropic multipliers), not published here,
		// so only in/out are pinned.
		{"anthropic.claude-opus-4-6-20251101-v1:0", 5.00, 0.50, 0, 25.00, "AWS Marketplace prodview-ssjdkfefxkn4i — Opus 4.6 Bedrock $5/$25 (was wrongly 17.25/86.25, a 3.45x OVER-bill)"},
		{"anthropic.claude-sonnet-4-6-20251101-v1:0", 3.00, 0.30, 0, 15.00, "AWS Marketplace prodview-o6w4hyizv7g64 — Sonnet 4.6 Bedrock $3/$15 (was wrongly 3.45/17.25, a 1.15x OVER-bill)"},

		// ── Google ── https://ai.google.dev/gemini-api/docs/pricing (fetched 2026-07-26), paid tier.
		// google gets NO cache discount in withCacheRates (cache read == input), so cachedIn == in.
		// CORRECTED: held 0.075/0.30, an older Flash generation's rate — 4x under in, 8.3x under out.
		{"gemini-2.5-flash", 0.30, 0.30, 0, 2.50, "gemini pricing: 2.5 Flash $0.30/$2.50 (was wrongly 0.075/0.30)"},
		// Correct for prompts <= 200k. Above that Google charges 2.50/15.00 and Lens does not model it.
		{"gemini-2.5-pro", 1.25, 1.25, 0, 10.00, "gemini pricing: 2.5 Pro, <=200k-token tier"},

		// ── Groq ── https://groq.com/pricing (fetched 2026-07-26). BOTH ALREADY CORRECT — pinned so
		// they stay that way. A verification that finds nothing wrong is still worth keeping.
		{"llama-3.3-70b-versatile", 0.59, 0.59, 0, 0.79, "groq pricing: llama-3.3-70b-versatile"},
		{"llama-3.1-8b-instant", 0.05, 0.05, 0, 0.08, "groq pricing: llama-3.1-8b-instant"},

		// ── Mistral ── https://mistral.ai/pricing (fetched 2026-07-26). ALREADY CORRECT.
		{"mistral-large-latest", 2.00, 2.00, 0, 6.00, "mistral pricing: Mistral Large $2/$6"},
	}
}

func TestPublishedRates(t *testing.T) {
	for _, c := range publishedRateCases() {
		in, cachedIn, write, out, ok := PriceDetailed(c.id)
		if !ok {
			t.Errorf("%s: NOT IN CATALOG — it would be served on the fallback rate (%s)", c.id, c.source)
			continue
		}
		if !nearly(in, c.in) || !nearly(out, c.out) {
			t.Errorf("%s: in/out = %v/%v, want %v/%v — %s", c.id, in, out, c.in, c.out, c.source)
		}
		if !nearly(cachedIn, c.cachedIn) {
			t.Errorf("%s: cache-read = %v, want %v — %s", c.id, cachedIn, c.cachedIn, c.source)
		}
		if c.write != 0 && !nearly(write, c.write) {
			t.Errorf("%s: cache-write = %v, want %v — %s", c.id, write, c.write, c.source)
		}
	}
}

// The literal ids Claude Code sends resolve to a priced row rather than falling through.
func TestLongContextVariantsResolve(t *testing.T) {
	// The published page: "Claude 4.6 and later models include the full 1M token context window at
	// standard pricing" — so a [1m] suffix is not a separate SKU and must alias, never duplicate.
	for _, id := range []string{"claude-opus-5[1m]", "claude-sonnet-5[1m]"} {
		in, _, _, out, ok := PriceDetailed(id)
		if !ok {
			t.Errorf("%s does not resolve — the id Claude Code actually sends would be served free", id)
			continue
		}
		if in <= 0 || out <= 0 {
			t.Errorf("%s resolved to a zero rate: in=%v out=%v", id, in, out)
		}
	}
}
