package proxy

import "testing"

// The usage-extraction seam reads the provider's REPORTED token counts so
// spend is billed on real usage instead of a len/4 estimate. Two wire
// shapes reach the spend site: OpenAI-shape (openai/mistral/groq/vllm, plus
// google+bedrock after reverse-translation) and Anthropic-native shape
// (anthropic has no translateResponse, so its body stays input_tokens/
// output_tokens). ExtractUsage dispatches on the provider name.

func TestExtractUsage_OpenAIShape(t *testing.T) {
	cfg := providerConfig{name: "openai"}
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":42,"completion_tokens":7,"total_tokens":49}}`)

	u, ok := cfg.ExtractUsage(body)
	if !ok {
		t.Fatal("usage present in OpenAI-shape response must be extracted")
	}
	if u.InputTokens != 42 || u.OutputTokens != 7 {
		t.Fatalf("got input=%d output=%d, want 42/7", u.InputTokens, u.OutputTokens)
	}
}

func TestExtractUsage_AnthropicShape(t *testing.T) {
	cfg := providerConfig{name: "anthropic"}
	// Anthropic native: usage.input_tokens / output_tokens (NOT prompt_tokens).
	body := []byte(`{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":31,"output_tokens":12}}`)

	u, ok := cfg.ExtractUsage(body)
	if !ok {
		t.Fatal("usage present in Anthropic-shape response must be extracted")
	}
	if u.InputTokens != 31 || u.OutputTokens != 12 {
		t.Fatalf("got input=%d output=%d, want 31/12", u.InputTokens, u.OutputTokens)
	}
}

func TestExtractUsage_AbsentReportsNotOK(t *testing.T) {
	// A response with no usage block (older/edge responses) must report
	// ok=false so the caller falls back to estimation — never an error,
	// never a zero billed silently as if it were real.
	cfg := providerConfig{name: "openai"}
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)

	if _, ok := cfg.ExtractUsage(body); ok {
		t.Fatal("missing usage block must report ok=false (fall back to estimate)")
	}
}

func TestExtractUsage_BedrockAndGoogleUseOpenAIShape(t *testing.T) {
	// google + bedrock are reverse-translated to OpenAI shape BEFORE the
	// spend site, so they extract via the OpenAI-shape path.
	body := []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	for _, name := range []string{"google", "bedrock", "mistral", "groq", "vllm"} {
		cfg := providerConfig{name: name}
		u, ok := cfg.ExtractUsage(body)
		if !ok || u.InputTokens != 5 || u.OutputTokens != 3 {
			t.Fatalf("%s: got (%d,%d ok=%v), want (5,3 true)", name, u.InputTokens, u.OutputTokens, ok)
		}
	}
}
