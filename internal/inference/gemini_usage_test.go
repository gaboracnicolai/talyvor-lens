package inference

import (
	"encoding/json"
	"testing"
)

// Relocated from internal/proxy/gemini_usage_test.go (PR-3c). TranslateFromGemini must carry Gemini's
// usageMetadata into the normalized OpenAI usage block so the shared extractor bills Gemini on its real
// reported counts. Both halves are now inference funcs (TranslateFromGemini + ExtractUsage); the
// providerConfig{name:"google"}.ExtractUsage seam call became ExtractUsage("google", out). Assertions
// byte-identical.
func TestTranslateFromGemini_CarriesUsageMetadata(t *testing.T) {
	gemini := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}],
		"usageMetadata":{"promptTokenCount":123,"candidatesTokenCount":45,"totalTokenCount":168}
	}`)

	out, err := TranslateFromGemini(gemini, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("translateFromGemini: %v", err)
	}

	var got struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal translated: %v", err)
	}
	if got.Usage.PromptTokens != 123 || got.Usage.CompletionTokens != 45 || got.Usage.TotalTokens != 168 {
		t.Fatalf("usage = %+v, want prompt=123 completion=45 total=168", got.Usage)
	}

	// And it must be extractable through the seam (provider "google").
	if u, ok := ExtractUsage("google", out); !ok || u.InputTokens != 123 || u.OutputTokens != 45 {
		t.Fatalf("ExtractUsage(google) = (%d,%d ok=%v), want (123,45 true)", u.InputTokens, u.OutputTokens, ok)
	}
}
