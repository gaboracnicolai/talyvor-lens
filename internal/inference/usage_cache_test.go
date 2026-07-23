package inference

import "testing"

// TestExtractUsage_OpenAICachedTokens: OpenAI reports cached tokens under
// usage.prompt_tokens_details.cached_tokens, and cached_tokens is a SUBSET of
// prompt_tokens (verified live docs). So the disjoint billable breakdown is
// uncached = prompt_tokens - cached_tokens, and the legacy InputTokens field stays
// prompt_tokens (unchanged for the old cost path).
func TestExtractUsage_OpenAICachedTokens(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10000,"completion_tokens":500,"prompt_tokens_details":{"cached_tokens":9000}}}`)

	u, ok := ExtractUsage("openai", body)
	if !ok {
		t.Fatal("usage must be extracted")
	}
	if u.InputTokens != 10000 { // legacy field UNCHANGED = prompt_tokens (incl cached)
		t.Errorf("InputTokens = %d, want 10000 (legacy prompt_tokens, unchanged)", u.InputTokens)
	}
	if u.UncachedInputTokens != 1000 { // 10000 - 9000
		t.Errorf("UncachedInputTokens = %d, want 1000", u.UncachedInputTokens)
	}
	if u.CachedInputTokens != 9000 {
		t.Errorf("CachedInputTokens = %d, want 9000", u.CachedInputTokens)
	}
	if u.CacheWriteInputTokens != 0 {
		t.Errorf("CacheWriteInputTokens = %d, want 0 (no cache_write_tokens pre-GPT-5.6)", u.CacheWriteInputTokens)
	}
	if u.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500", u.OutputTokens)
	}
}

// TestExtractUsage_AnthropicCacheReadWrite: Anthropic reports cache reads/writes
// as SEPARATE fields (cache_read_input_tokens, cache_creation_input_tokens); its
// input_tokens is the uncached-only count, disjoint from them (verified live docs:
// {"input_tokens":50,"cache_read_input_tokens":100000,"cache_creation_input_tokens":248}).
func TestExtractUsage_AnthropicCacheReadWrite(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":9000,"cache_creation_input_tokens":248}}`)

	u, ok := ExtractUsage("anthropic", body)
	if !ok {
		t.Fatal("usage must be extracted")
	}
	if u.InputTokens != 1000 { // legacy field UNCHANGED = input_tokens (uncached only for Anthropic)
		t.Errorf("InputTokens = %d, want 1000 (legacy input_tokens, unchanged)", u.InputTokens)
	}
	if u.UncachedInputTokens != 1000 { // Anthropic input_tokens IS the uncached count
		t.Errorf("UncachedInputTokens = %d, want 1000", u.UncachedInputTokens)
	}
	if u.CachedInputTokens != 9000 {
		t.Errorf("CachedInputTokens = %d, want 9000", u.CachedInputTokens)
	}
	if u.CacheWriteInputTokens != 248 {
		t.Errorf("CacheWriteInputTokens = %d, want 248", u.CacheWriteInputTokens)
	}
	if u.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500", u.OutputTokens)
	}
}

// TestExtractUsage_NoCacheAccountingIsUnchanged: a response with no cache fields
// (the common case, and every response before this) must report the full input as
// uncached and zero cached/write — so cache-aware pricing collapses to the old
// full-rate cost. Behaviour is unchanged where the provider reports no cache.
func TestExtractUsage_NoCacheAccountingIsUnchanged(t *testing.T) {
	for name, body := range map[string][]byte{
		"openai":    []byte(`{"usage":{"prompt_tokens":42,"completion_tokens":7}}`),
		"anthropic": []byte(`{"usage":{"input_tokens":31,"output_tokens":12}}`),
	} {
		u, ok := ExtractUsage(name, body)
		if !ok {
			t.Fatalf("%s: usage must be extracted", name)
		}
		if u.UncachedInputTokens != u.InputTokens {
			t.Errorf("%s: UncachedInputTokens=%d must equal InputTokens=%d when no cache reported", name, u.UncachedInputTokens, u.InputTokens)
		}
		if u.CachedInputTokens != 0 || u.CacheWriteInputTokens != 0 {
			t.Errorf("%s: cached=%d write=%d, both must be 0 when no cache reported", name, u.CachedInputTokens, u.CacheWriteInputTokens)
		}
	}
}
