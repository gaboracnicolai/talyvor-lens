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

	return []Model{
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
		{ID: "claude-haiku-4-6", Provider: "anthropic", DisplayName: "Claude Haiku 4.6", InputPer1M: 0.80, OutputPer1M: 4.00, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},
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
		{ID: "anthropic.claude-haiku-4-6-20251103-v1:0", Provider: "bedrock", DisplayName: "Claude Haiku 4.6 (Bedrock)", InputPer1M: 0.92, OutputPer1M: 4.60, Capabilities: visionDoc, ContextTokens: 200000, MaxOutput: 8192},

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
	}
}
