package inference

import "encoding/json"

// Usage is a provider's REPORTED token accounting for one response. Moved from internal/proxy (PR-3b A′);
// proxy keeps a `type Usage = inference.Usage` alias so its spend sites stay unedited.
//
// InputTokens/OutputTokens are the legacy fields, UNCHANGED: InputTokens is exactly what the provider
// reports as input — OpenAI's prompt_tokens (which INCLUDES cached tokens) or Anthropic's input_tokens
// (which EXCLUDES cache read/write, reported separately). Existing callers + the old CostUSD path read
// these and are unaffected.
//
// The Uncached/Cached/CacheWrite fields are the cache-aware DISJOINT breakdown (added 2026-07): the true
// billable input is UncachedInputTokens + CachedInputTokens + CacheWriteInputTokens, each priced at its
// own catalog rate (see alerts.CostUSDDetailed). They are normalized here so pricing is provider-agnostic.
// When the provider reports no cache accounting, Uncached == InputTokens and the other two are 0, so
// cache-aware pricing collapses to the old full-rate cost.
type Usage struct {
	InputTokens  int
	OutputTokens int

	UncachedInputTokens   int // input billed at the full input rate
	CachedInputTokens     int // cache READS — billed at the cache-read rate
	CacheWriteInputTokens int // cache WRITES — billed at the cache-write rate
}

// ExtractUsage reads the provider's reported usage out of an upstream response body (ok=false when no
// usage block is present — the caller then estimates). It dispatches on the provider NAME (a single
// source of truth) rather than a per-config closure: by the spend site openai/mistral/groq/vllm pass
// through native OpenAI shape and google+bedrock are reverse-translated to OpenAI shape, so all read
// usage.prompt_tokens/completion_tokens; anthropic alone stays native (usage.input_tokens/output_tokens).
func ExtractUsage(provider string, body []byte) (Usage, bool) {
	if provider == "anthropic" {
		return extractAnthropicUsage(body)
	}
	return extractOpenAIUsage(body)
}

// extractOpenAIUsage reads the OpenAI-shape usage object. The pointer makes "field absent" distinguishable
// from "present but zero" so a missing block reports ok=false rather than billing a silent zero.
//
// OpenAI reports cached tokens under usage.prompt_tokens_details.cached_tokens, and cached_tokens is a
// SUBSET of prompt_tokens (the total). cache_write_tokens appears only for GPT-5.6+ (0 otherwise; no
// catalog model is 5.6+). So the disjoint billable breakdown is uncached = prompt_tokens - cached - write,
// clamped at 0 defensively. The legacy InputTokens stays prompt_tokens (the total), unchanged.
func extractOpenAIUsage(body []byte) (Usage, bool) {
	var r struct {
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Usage == nil {
		return Usage{}, false
	}
	cached, write := 0, 0
	if d := r.Usage.PromptTokensDetails; d != nil {
		cached, write = d.CachedTokens, d.CacheWriteTokens
	}
	uncached := r.Usage.PromptTokens - cached - write // cached/write are a subset of prompt_tokens
	if uncached < 0 {
		uncached = 0
	}
	return Usage{
		InputTokens:           r.Usage.PromptTokens, // legacy: total input incl cached — UNCHANGED
		OutputTokens:          r.Usage.CompletionTokens,
		UncachedInputTokens:   uncached,
		CachedInputTokens:     cached,
		CacheWriteInputTokens: write,
	}, true
}

// extractAnthropicUsage reads the Anthropic-native usage object. Anthropic reports cache reads and writes
// as SEPARATE fields (cache_read_input_tokens, cache_creation_input_tokens); input_tokens is the uncached
// count, DISJOINT from them (verified: {"input_tokens":50,"cache_read_input_tokens":100000,
// "cache_creation_input_tokens":248}). So uncached is exactly input_tokens; the legacy InputTokens stays
// input_tokens (unchanged — it never included cache read/write).
func extractAnthropicUsage(body []byte) (Usage, bool) {
	var r struct {
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Usage == nil {
		return Usage{}, false
	}
	return Usage{
		InputTokens:           r.Usage.InputTokens, // legacy: uncached only (cache read/write are separate) — UNCHANGED
		OutputTokens:          r.Usage.OutputTokens,
		UncachedInputTokens:   r.Usage.InputTokens,
		CachedInputTokens:     r.Usage.CacheReadInputTokens,
		CacheWriteInputTokens: r.Usage.CacheCreationInputTokens,
	}, true
}
