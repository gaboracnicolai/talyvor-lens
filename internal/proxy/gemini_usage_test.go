package proxy

import (
	"encoding/json"
	"testing"

	"github.com/talyvor/lens/internal/inference"
)

// TranslateFromGemini must carry Gemini's usageMetadata into the normalized OpenAI usage block so the
// (shared) usage extractor bills Gemini on its real reported counts. This stays in package proxy because
// it also asserts the providerConfig.ExtractUsage SEAM (which stays in proxy, A′); only the moved-func
// reference (translateFromGemini → inference.TranslateFromGemini) updated.
func TestTranslateFromGemini_CarriesUsageMetadata(t *testing.T) {
	gemini := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}],
		"usageMetadata":{"promptTokenCount":123,"candidatesTokenCount":45,"totalTokenCount":168}
	}`)

	out, err := inference.TranslateFromGemini(gemini, "gemini-2.5-flash")
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
	if u, ok := (providerConfig{name: "google"}).ExtractUsage(out); !ok || u.InputTokens != 123 || u.OutputTokens != 45 {
		t.Fatalf("ExtractUsage(google) = (%d,%d ok=%v), want (123,45 true)", u.InputTokens, u.OutputTokens, ok)
	}
}
