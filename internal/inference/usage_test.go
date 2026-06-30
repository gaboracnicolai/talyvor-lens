package inference

import "testing"

// Relocated from internal/proxy/usage_test.go (PR-3c) — these are usage-PARSING unit tests; the dispatch
// logic is inference.ExtractUsage. Construction-only change: providerConfig{name:X}.ExtractUsage(body) →
// ExtractUsage("X", body). The OpenAI/Anthropic wire-shape assertions are byte-identical.

func TestExtractUsage_OpenAIShape(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":42,"completion_tokens":7,"total_tokens":49}}`)

	u, ok := ExtractUsage("openai", body)
	if !ok {
		t.Fatal("usage present in OpenAI-shape response must be extracted")
	}
	if u.InputTokens != 42 || u.OutputTokens != 7 {
		t.Fatalf("got input=%d output=%d, want 42/7", u.InputTokens, u.OutputTokens)
	}
}

func TestExtractUsage_AnthropicShape(t *testing.T) {
	// Anthropic native: usage.input_tokens / output_tokens (NOT prompt_tokens).
	body := []byte(`{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":31,"output_tokens":12}}`)

	u, ok := ExtractUsage("anthropic", body)
	if !ok {
		t.Fatal("usage present in Anthropic-shape response must be extracted")
	}
	if u.InputTokens != 31 || u.OutputTokens != 12 {
		t.Fatalf("got input=%d output=%d, want 31/12", u.InputTokens, u.OutputTokens)
	}
}

func TestExtractUsage_AbsentReportsNotOK(t *testing.T) {
	// A response with no usage block (older/edge responses) must report ok=false so the caller falls back
	// to estimation — never an error, never a zero billed silently as if it were real.
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)

	if _, ok := ExtractUsage("openai", body); ok {
		t.Fatal("missing usage block must report ok=false (fall back to estimate)")
	}
}

func TestExtractUsage_BedrockAndGoogleUseOpenAIShape(t *testing.T) {
	// google + bedrock are reverse-translated to OpenAI shape BEFORE the spend site, so they extract via
	// the OpenAI-shape path.
	body := []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	for _, name := range []string{"google", "bedrock", "mistral", "groq", "vllm"} {
		u, ok := ExtractUsage(name, body)
		if !ok || u.InputTokens != 5 || u.OutputTokens != 3 {
			t.Fatalf("%s: got (%d,%d ok=%v), want (5,3 true)", name, u.InputTokens, u.OutputTokens, ok)
		}
	}
}
