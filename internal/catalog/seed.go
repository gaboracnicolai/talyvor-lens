package catalog

// seedModels is the embedded default catalog. PRICING IS MIGRATED
// BYTE-FOR-BYTE from the previous sources — alerts.modelPrices for the rates
// and modality.capabilities for the vision/audio/document flags. Do not
// "tidy" a number here: the price-parity test (golden_test.go) asserts every
// existing model still prices identically, because a silent price drift
// corrupts every budget/forecast/anomaly/ROI figure (the cost moat).
//
// ContextTokens/MaxOutput are best-effort informational values (nothing gates
// on them yet); pricing + capabilities are authoritative.
func seedModels() []Model {
	vision := Capabilities{Vision: true}
	visionDoc := Capabilities{Vision: true, Document: true}
	visionAudioDoc := Capabilities{Vision: true, Audio: true, Document: true}

	return withCacheRates([]Model{
		// ─── OpenAI (vision) ───
		{ID: "gpt-4o", Provider: "openai", DisplayName: "GPT-4o", InputPer1M: 2.50, OutputPer1M: 10.00, Capabilities: vision, ContextTokens: 128000, MaxOutput: 16384, Aliases: []string{"gpt-4o-2024-11-20", "gpt-4o-2024-08-06"}},
		{ID: "gpt-4o-mini", Provider: "openai", DisplayName: "GPT-4o mini", InputPer1M: 0.15, OutputPer1M: 0.60, Capabilities: vision, ContextTokens: 128000, MaxOutput: 16384, Aliases: []string{"gpt-4o-mini-2024-07-18"}},
		{ID: "gpt-4.1-nano", Provider: "openai", DisplayName: "GPT-4.1 nano", InputPer1M: 0.10, OutputPer1M: 0.40, Capabilities: vision, ContextTokens: 1000000, MaxOutput: 32768},
		{ID: "gpt-5.4", Provider: "openai", DisplayName: "GPT-5.4", InputPer1M: 2.50, OutputPer1M: 15.00, CachedInputPer1M: 0.25, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		{ID: "gpt-5.4-mini", Provider: "openai", DisplayName: "GPT-5.4 mini", InputPer1M: 0.75, OutputPer1M: 4.50, CachedInputPer1M: 0.075, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		// ⚠ THE THREE BELOW WERE ON THE PRICING PAGE AND NOT IN THIS CATALOG. Every rate transcribed
		// from https://developers.openai.com/api/docs/pricing (fetched 2026-07-28), Standard tier.
		// Cached-input is the page's own "Cached" column in each case — 0.1x input for this generation,
		// NOT the 0.5x withCacheRates would otherwise derive for openai, so each is set explicitly.
		// ContextTokens/MaxOutput mirror their siblings and are informational only (nothing gates on
		// them); the PRICES are the authoritative part and are the only figures taken from the page.
		{ID: "gpt-5.4-nano", Provider: "openai", DisplayName: "GPT-5.4 nano", InputPer1M: 0.20, OutputPer1M: 1.25, CachedInputPer1M: 0.02, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		// gpt-5.3-codex is the one that matters most in practice: talyvor-code sends source code, and a
		// coding-specialised model is what such a client reaches for. 35x under-recovery on output.
		{ID: "gpt-5.3-codex", Provider: "openai", DisplayName: "GPT-5.3 Codex", InputPer1M: 1.75, OutputPer1M: 14.00, CachedInputPer1M: 0.175, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		// ⚠ chat-latest is priced by OpenAI as its own SKU, so this row bills what the page publishes
		// for that id — it is NOT an attempt to track whatever model the pointer currently resolves to.
		// It also has no "gpt-" prefix, so providerFromID could not infer its provider and the derived
		// floor was taken across the WHOLE catalog (mistral-nemo, $0.025/1M): a measured 1200x
		// under-recovery on output, the largest of any id here. That fallback is behaving as designed —
		// a CHARGE deliberately falls back low so a guessed rate never over-bills — which is exactly
		// why the remedy is to price the model rather than to change the fallback.
		{ID: "chat-latest", Provider: "openai", DisplayName: "GPT chat-latest", InputPer1M: 5.00, OutputPer1M: 30.00, CachedInputPer1M: 0.50, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		{ID: "gpt-4.1", Provider: "openai", DisplayName: "GPT-4.1", InputPer1M: 2.00, OutputPer1M: 8.00, Capabilities: vision, ContextTokens: 1000000, MaxOutput: 32768},
		{ID: "gpt-4.1-mini", Provider: "openai", DisplayName: "GPT-4.1 mini", InputPer1M: 0.40, OutputPer1M: 1.60, Capabilities: vision, ContextTokens: 1000000, MaxOutput: 32768},

		// ─── OpenAI GPT-5.5 / 5.6 ─────────────────────────────────────────────────────────────────
		//
		// ⚠ EVERY RATE BELOW IS TRANSCRIBED FROM https://developers.openai.com/api/docs/pricing
		//   (fetched 2026-07-26), Standard tier. Cached-input rates are the page's own "Cached" column —
		//   0.1x input for these generations, NOT the 0.5x withCacheRates assumes for openai, so they are
		//   set explicitly.
		{ID: "gpt-5.6-sol", Provider: "openai", DisplayName: "GPT-5.6 Sol", InputPer1M: 5.00, OutputPer1M: 30.00, CachedInputPer1M: 0.50, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		{ID: "gpt-5.6-terra", Provider: "openai", DisplayName: "GPT-5.6 Terra", InputPer1M: 2.50, OutputPer1M: 15.00, CachedInputPer1M: 0.25, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		{ID: "gpt-5.6-luna", Provider: "openai", DisplayName: "GPT-5.6 Luna", InputPer1M: 1.00, OutputPer1M: 6.00, CachedInputPer1M: 0.10, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		{ID: "gpt-5.5", Provider: "openai", DisplayName: "GPT-5.5", InputPer1M: 5.00, OutputPer1M: 30.00, CachedInputPer1M: 0.50, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		// The -pro rows publish "—" in the Cached column: no cached-input discount is offered. Setting
		// CachedInputPer1M == InputPer1M encodes that honestly; leaving it 0 would let withCacheRates
		// invent a 0.5x discount that does not exist and under-bill every cache read.
		{ID: "gpt-5.5-pro", Provider: "openai", DisplayName: "GPT-5.5 Pro", InputPer1M: 30.00, OutputPer1M: 180.00, CachedInputPer1M: 30.00, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		{ID: "gpt-5.4-pro", Provider: "openai", DisplayName: "GPT-5.4 Pro", InputPer1M: 30.00, OutputPer1M: 180.00, CachedInputPer1M: 30.00, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		// ⚠ NOT TOUCHED: gpt-4o, gpt-4o-mini, gpt-4.1, gpt-4.1-mini, gpt-4.1-nano. They are ABSENT from
		// the current pricing page, which does not mean they are unavailable — it may mean the page lists
		// only current models. Their rates here are therefore UNVERIFIED against a published source as of
		// 2026-07-26. They are left exactly as they were rather than guessed at or deleted: no published
		// price, no edit. Re-verify before relying on them.

		// ─── Anthropic (vision + document) ───
		{ID: "claude-opus-4-5", Provider: "anthropic", DisplayName: "Claude Opus 4.5", InputPer1M: 5.00, OutputPer1M: 25.00, CachedInputPer1M: 0.50, CacheWritePer1M: 6.25, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192, Aliases: []string{"claude-opus-4-5-20251101"}},
		{ID: "claude-sonnet-4-5", Provider: "anthropic", DisplayName: "Claude Sonnet 4.5", InputPer1M: 3.00, OutputPer1M: 15.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192, Aliases: []string{"claude-sonnet-4-5-20250929"}},
		{ID: "claude-haiku-4-5", Provider: "anthropic", DisplayName: "Claude Haiku 4.5", InputPer1M: 1.00, OutputPer1M: 5.00, CachedInputPer1M: 0.10, CacheWritePer1M: 1.25, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192, Aliases: []string{"claude-haiku-4-5-20251001"}},

		// ⚠ THE DATED IDS ABOVE ARE THE PROVIDER'S PRIMARY IDS, NOT VARIANTS. Anthropic's model table
		// lists the dated form as the "Claude API ID" and the bare name as a convenience alias;
		// GET /v1/models returns the dated form, and Anthropic's docs recommend pinning a snapshot.
		// Without these aliases a client following that advice was billed on the derived floor — 5x
		// under on Opus 4.5, 3x under on Sonnet 4.5 — while its requests looked entirely ordinary.
		// The alias mechanism was already here and already used for OpenAI's dated snapshots
		// (gpt-4o-2024-11-20); it had simply never been applied to Anthropic's.
		// https://platform.claude.com/docs/en/about-claude/models/overview (fetched 2026-07-28)

		// Claude Opus 4.1 — DEPRECATED, retires 2026-08-05, still callable first-party until then.
		// $15/$75 against a $1/$5 floor is the largest single under-recovery in the Anthropic set (15x).
		// Rates transcribed from https://platform.claude.com/docs/en/about-claude/pricing (2026-07-28):
		// base input $15, 5m cache write $18.75, cache hit $1.50, output $75.
		{ID: "claude-opus-4-1", Provider: "anthropic", DisplayName: "Claude Opus 4.1", InputPer1M: 15.00, OutputPer1M: 75.00, CachedInputPer1M: 1.50, CacheWritePer1M: 18.75, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 32768, Aliases: []string{"claude-opus-4-1-20250805"}},
		{ID: "claude-opus-4-6", Provider: "anthropic", DisplayName: "Claude Opus 4.6", InputPer1M: 5.00, OutputPer1M: 25.00, CachedInputPer1M: 0.50, CacheWritePer1M: 6.25, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
		{ID: "claude-sonnet-4-6", Provider: "anthropic", DisplayName: "Claude Sonnet 4.6", InputPer1M: 3.00, OutputPer1M: 15.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
		// NOTE: there is deliberately NO "claude-haiku-4-6" — no Haiku 4.6 exists at any version
		// (verified against GET /v1/models). The real cheapest Anthropic model is claude-haiku-4-5
		// above; a phantom entry here 404s the first cost-routed request. Guarded by
		// verified_models_test.go.
		// Claude Opus 4.8 (Upgrade 16) — verified $5 in / $25 out per 1M,
		// vision-capable, 200K context. Selectable but NOT wired into any
		// routing default (router tiers unchanged), so it's never
		// auto-selected — a caller must pin it.
		{ID: "claude-opus-4-8", Provider: "anthropic", DisplayName: "Claude Opus 4.8", InputPer1M: 5.00, OutputPer1M: 25.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},

		// ─── The Claude 5 family + Opus 4.7 ────────────────────────────────────────────────────────
		//
		// ⚠ EVERY RATE BELOW IS TRANSCRIBED FROM THE PUBLISHED TABLE AT
		//   https://platform.claude.com/docs/en/about-claude/pricing   (fetched 2026-07-26)
		// Nothing here is inferred from the -4 series, interpolated, or taken second-hand. A guessed rate
		// in this table is WORSE than the unknown-model fallback, because the fallback is honest about
		// being a floor while a wrong entry here is silently authoritative.
		//
		// Cache columns come from the same table's "Cache Hits & Refreshes" (read) and "5m Cache Writes"
		// (write) columns, set EXPLICITLY so withCacheRates' provider multipliers do not derive them.
		// The 1h cache write (2x input) is not modelled: cache_creation_input_tokens cannot be split by
		// TTL, so the existing convention prices all writes at the 5m rate — see withCacheRates.
		{ID: "claude-opus-5", Provider: "anthropic", DisplayName: "Claude Opus 5", InputPer1M: 5.00, OutputPer1M: 25.00, CachedInputPer1M: 0.50, CacheWritePer1M: 6.25, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 8192, Aliases: []string{"claude-opus-5[1m]"}},
		{ID: "claude-opus-4-7", Provider: "anthropic", DisplayName: "Claude Opus 4.7", InputPer1M: 5.00, OutputPer1M: 25.00, CachedInputPer1M: 0.50, CacheWritePer1M: 6.25, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 8192},
		// ⚠⚠ SONNET 5 IS ON INTRODUCTORY PRICING AND IT ENDS. The published table lists TWO rows:
		//   "Claude Sonnet 5 through August 31, 2026"      → $2 in / $10 out  (cache read $0.20, write $2.50)
		//   "Claude Sonnet 5 starting September 1, 2026"    → $3 in / $15 out  (cache read $0.30, write $3.75)
		// The entry below is the rate in force TODAY. ON 2026-09-01 IT MUST BECOME 3.00 / 15.00 / 0.30 /
		// 3.75. Entering the higher standard rate now would over-bill every Sonnet 5 request for five
		// weeks, which is the one direction this repo does not accept; entering the introductory rate
		// means Lens UNDER-bills after 1 September until someone edits this line. That is the safe
		// direction, and it is a DATED action item — nothing in the code can detect a price change on an
		// id that already exists (see model_detection.go's stated limits).
		{ID: "claude-sonnet-5", Provider: "anthropic", DisplayName: "Claude Sonnet 5", InputPer1M: 2.00, OutputPer1M: 10.00, CachedInputPer1M: 0.20, CacheWritePer1M: 2.50, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 8192, Aliases: []string{"claude-sonnet-5[1m]"}},
		{ID: "claude-fable-5", Provider: "anthropic", DisplayName: "Claude Fable 5", InputPer1M: 10.00, OutputPer1M: 50.00, CachedInputPer1M: 1.00, CacheWritePer1M: 12.50, Capabilities: visionDoc, ContextTokens: 1000000, MaxOutput: 8192},
		// claude-mythos-5 IS DELIBERATELY NOT SEEDED. Its rate IS published ($10/$50, same as Fable 5),
		// so this is not a missing-price case — it is a missing-EXISTENCE case. The page marks it
		// "limited availability" and it is absent from the pinned /v1/models capture that
		// verified_models_test.go guards, so I cannot show it is dispatchable. Seeding a model that may
		// not exist is the phantom-claude-haiku-4-6 bug, which 404'd a live request. Unpriced is now
		// SAFE (the fallback charges a floor and alerts) whereas phantom is not, so the honest order is
		// alert-then-price. Add it here the moment a /v1/models capture shows it.
		// NO SEPARATE 1M-CONTEXT ENTRY, deliberately. The same page states: "Claude 4.6 and later models
		// include the full 1M token context window at standard pricing. (A 900k-token request is billed at
		// the same per-token rate as a 9k-token request.)" So a "[1m]" suffix is not a distinct SKU. A
		// client that sends the literal id "claude-opus-5[1m]" still misses this table — resolved by the
		// alias below rather than by a duplicate priced row.

		// ─── Google Gemini (vision + audio + document) ───
		// VERIFIED CORRECT for prompts <= 200k tokens (1.25 / 10.00), same source, same date.
		// ⚠ UNMODELLED TIER: above 200k tokens Google charges 2.50 / 15.00 — double. Lens has no
		// prompt-size-dependent rate, so long-context requests to this model UNDER-bill 2x. Same
		// class as Anthropic's fast mode: a price that varies by request shape, not by model id.
		{ID: "gemini-2.5-pro", Provider: "google", DisplayName: "Gemini 2.5 Pro", InputPer1M: 1.25, OutputPer1M: 10.00, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},
		// ⚠ CORRECTED 2026-07-26 — UNDER-BILLING BADLY. Held 0.075/0.30 (an older Flash generation's
		// rate); published is 0.30 in / 2.50 out — 4x under on input and 8.3x under on OUTPUT.
		// Source: https://ai.google.dev/gemini-api/docs/pricing (fetched 2026-07-26), paid tier.
		// The $1.00 audio-input tier is NOT modelled — Lens has no per-modality input rate, so
		// audio prompts to this model still bill at the text rate. Flagged, not guessed.
		{ID: "gemini-2.5-flash", Provider: "google", DisplayName: "Gemini 2.5 Flash", InputPer1M: 0.30, OutputPer1M: 2.50, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},
		{ID: "gemini-2.0-flash", Provider: "google", DisplayName: "Gemini 2.0 Flash", InputPer1M: 0.10, OutputPer1M: 0.40, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},
		{ID: "gemini-1.5-pro", Provider: "google", DisplayName: "Gemini 1.5 Pro", InputPer1M: 1.25, OutputPer1M: 5.00, Capabilities: visionAudioDoc, ContextTokens: 2000000, MaxOutput: 8192},
		{ID: "gemini-1.5-flash", Provider: "google", DisplayName: "Gemini 1.5 Flash", InputPer1M: 0.075, OutputPer1M: 0.30, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},

		// ─── AWS Bedrock Claude (vision + document; ~15% markup) ───
		// ⚠ CORRECTED 2026-07-26 — THESE WERE OVER-BILLING, AND THE MARKUP WAS INVENTED.
		// They held 17.25/86.25 and 3.45/17.25, which are EXACTLY 1.15x the direct Anthropic rates
		// (17.25 == 15.00 x 1.15, 86.25 == 75.00 x 1.15). Somebody assumed Bedrock charges a 15%
		// premium and derived these instead of reading AWS's price. Two errors compounded: the 15%
		// was fictional, AND the direct rate it was applied to was itself wrong (Opus 4.6 was
		// carrying Opus 4.1's 15/75 — see the corrections above).
		//
		// AWS publishes the SAME per-token rate as Anthropic direct — no premium at all:
		//   Opus 4.6:   $5 in / $25 out   https://aws.amazon.com/marketplace/pp/prodview-ssjdkfefxkn4i
		//   Sonnet 4.6: $3 in / $15 out   https://aws.amazon.com/marketplace/pp/prodview-o6w4hyizv7g64
		//   (AWS Marketplace "Bedrock Edition" listings, fetched 2026-07-26. The main
		//    aws.amazon.com/bedrock/pricing page does NOT list the 4.6 models at all.)
		// So Opus-on-Bedrock was billed 3.45x its real cost and Sonnet 1.15x.
		//
		// Cache rates stay DERIVED (withCacheRates treats bedrock with the anthropic multipliers).
		// The Marketplace listing shows separate cache and batch line items which I did not
		// transcribe, so those remain an approximation — flagged rather than guessed at.
		{ID: "anthropic.claude-opus-4-6-20251101-v1:0", Provider: "bedrock", DisplayName: "Claude Opus 4.6 (Bedrock)", InputPer1M: 5.00, OutputPer1M: 25.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
		{ID: "anthropic.claude-sonnet-4-6-20251101-v1:0", Provider: "bedrock", DisplayName: "Claude Sonnet 4.6 (Bedrock)", InputPer1M: 3.00, OutputPer1M: 15.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
		// NOTE: no Bedrock "claude-haiku-4-6" twin either — the underlying Haiku 4.6 does not exist.

		// ─── Mistral (text-only) ───
		{ID: "mistral-large-latest", Provider: "mistral", DisplayName: "Mistral Large", InputPer1M: 2.00, OutputPer1M: 6.00, ContextTokens: 128000, MaxOutput: 8192},
		{ID: "mistral-small-latest", Provider: "mistral", DisplayName: "Mistral Small", InputPer1M: 0.10, OutputPer1M: 0.30, ContextTokens: 128000, MaxOutput: 8192},
		{ID: "mistral-nemo", Provider: "mistral", DisplayName: "Mistral NeMo", InputPer1M: 0.015, OutputPer1M: 0.045, ContextTokens: 128000, MaxOutput: 8192},
		{ID: "open-mistral-7b", Provider: "mistral", DisplayName: "Open Mistral 7B", InputPer1M: 0.025, OutputPer1M: 0.025, ContextTokens: 32000, MaxOutput: 8192},

		// ─── Groq (text-only, hardware-accelerated open weights) ───
		{ID: "llama-3.3-70b-versatile", Provider: "groq", DisplayName: "Llama 3.3 70B (Groq)", InputPer1M: 0.59, OutputPer1M: 0.79, ContextTokens: 128000, MaxOutput: 32768},
		{ID: "llama-3.1-8b-instant", Provider: "groq", DisplayName: "Llama 3.1 8B Instant (Groq)", InputPer1M: 0.05, OutputPer1M: 0.08, ContextTokens: 128000, MaxOutput: 8192},
		{ID: "mixtral-8x7b-32768", Provider: "groq", DisplayName: "Mixtral 8x7B (Groq)", InputPer1M: 0.24, OutputPer1M: 0.24, ContextTokens: 32768, MaxOutput: 8192},
		{ID: "gemma2-9b-it", Provider: "groq", DisplayName: "Gemma 2 9B (Groq)", InputPer1M: 0.20, OutputPer1M: 0.20, ContextTokens: 8192, MaxOutput: 8192},
	})
}

// withCacheRates fills each model's prompt-caching rates (CachedInputPer1M,
// CacheWritePer1M) from its provider's PUBLISHED multiplier on the base input
// rate, leaving InputPer1M/OutputPer1M byte-for-byte untouched (the price-parity
// gate). Rates as verified against the live provider docs on 2026-07-24:
//
//   - anthropic / bedrock (Claude economics — platform.claude.com prompt-caching):
//     cache READ = 0.1x input; 5-minute cache WRITE = 1.25x input. (A 1-hour
//     write is 2x, but the aggregate cache_creation_input_tokens field can't be
//     split by TTL, so we price at the default-TTL 1.25x; a 1-hour-cached write
//     is therefore slightly under-priced — the safe direction for a savings claim.)
//   - openai (developers.openai.com prompt-caching): cache READ ~0.5x input for
//     the GPT-4o generation. (GPT-4.1-gen is actually 0.25x, so 0.5x UNDER-states
//     the discount — deliberately conservative so we never over-claim savings.)
//     No separate write charge before GPT-5.6, and no catalog model is 5.6+.
//   - everything else (google/mistral/groq): prompt caching is not billed through
//     this cost path (and our usage parser reads no cache counts for them), so we
//     apply NO discount — cache read == input rate. Never under-bills.
//
// A model may still override these by carrying explicit non-zero values.
func withCacheRates(models []Model) []Model {
	for i := range models {
		m := &models[i]
		var cachedMult, writeMult float64
		switch m.Provider {
		case "anthropic", "bedrock":
			cachedMult, writeMult = 0.10, 1.25
		case "openai":
			cachedMult, writeMult = 0.50, 1.00
		default:
			cachedMult, writeMult = 1.00, 1.00
		}
		if m.CachedInputPer1M == 0 {
			m.CachedInputPer1M = m.InputPer1M * cachedMult
		}
		if m.CacheWritePer1M == 0 {
			m.CacheWritePer1M = m.InputPer1M * writeMult
		}
	}
	return models
}
