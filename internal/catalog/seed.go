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
		{ID: "gpt-5.4", Provider: "openai", DisplayName: "GPT-5.4", InputPer1M: 5.00, OutputPer1M: 20.00, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		{ID: "gpt-5.4-mini", Provider: "openai", DisplayName: "GPT-5.4 mini", InputPer1M: 0.50, OutputPer1M: 2.00, Capabilities: vision, ContextTokens: 400000, MaxOutput: 128000},
		{ID: "gpt-4.1", Provider: "openai", DisplayName: "GPT-4.1", InputPer1M: 2.00, OutputPer1M: 8.00, Capabilities: vision, ContextTokens: 1000000, MaxOutput: 32768},
		{ID: "gpt-4.1-mini", Provider: "openai", DisplayName: "GPT-4.1 mini", InputPer1M: 0.40, OutputPer1M: 1.60, Capabilities: vision, ContextTokens: 1000000, MaxOutput: 32768},

		// ─── Anthropic (vision + document) ───
		{ID: "claude-opus-4-5", Provider: "anthropic", DisplayName: "Claude Opus 4.5", InputPer1M: 15.00, OutputPer1M: 75.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
		{ID: "claude-sonnet-4-5", Provider: "anthropic", DisplayName: "Claude Sonnet 4.5", InputPer1M: 3.00, OutputPer1M: 15.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
		{ID: "claude-haiku-4-5", Provider: "anthropic", DisplayName: "Claude Haiku 4.5", InputPer1M: 0.80, OutputPer1M: 4.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
		{ID: "claude-opus-4-6", Provider: "anthropic", DisplayName: "Claude Opus 4.6", InputPer1M: 15.00, OutputPer1M: 75.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
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

		// ─── Google Gemini (vision + audio + document) ───
		{ID: "gemini-2.5-pro", Provider: "google", DisplayName: "Gemini 2.5 Pro", InputPer1M: 1.25, OutputPer1M: 10.00, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},
		{ID: "gemini-2.5-flash", Provider: "google", DisplayName: "Gemini 2.5 Flash", InputPer1M: 0.075, OutputPer1M: 0.30, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},
		{ID: "gemini-2.0-flash", Provider: "google", DisplayName: "Gemini 2.0 Flash", InputPer1M: 0.10, OutputPer1M: 0.40, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},
		{ID: "gemini-1.5-pro", Provider: "google", DisplayName: "Gemini 1.5 Pro", InputPer1M: 1.25, OutputPer1M: 5.00, Capabilities: visionAudioDoc, ContextTokens: 2000000, MaxOutput: 8192},
		{ID: "gemini-1.5-flash", Provider: "google", DisplayName: "Gemini 1.5 Flash", InputPer1M: 0.075, OutputPer1M: 0.30, Capabilities: visionAudioDoc, ContextTokens: 1000000, MaxOutput: 8192},

		// ─── AWS Bedrock Claude (vision + document; ~15% markup) ───
		{ID: "anthropic.claude-opus-4-6-20251101-v1:0", Provider: "bedrock", DisplayName: "Claude Opus 4.6 (Bedrock)", InputPer1M: 17.25, OutputPer1M: 86.25, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
		{ID: "anthropic.claude-sonnet-4-6-20251101-v1:0", Provider: "bedrock", DisplayName: "Claude Sonnet 4.6 (Bedrock)", InputPer1M: 3.45, OutputPer1M: 17.25, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
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
