package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestConfigForKey_CredentialSubstitution_Characterization pins the four-branch credential map directly
// against inference.ConfigForKey — the side that now OWNS the key-pool override logic (moved from
// proxy.applyKey in PR-3c). It is the durable, never-edited companion to the proxy-side #258 oracle:
// observed via the exported ApplyAuth/UpstreamURL methods (no unexported-field reach), so it survives any
// future consumer change. CONFIGURED keys are distinct sentinels from the POOLED key so a leak is
// detectable; the pooled key must be substituted, never the configured one.
func TestConfigForKey_CredentialSubstitution_Characterization(t *testing.T) {
	ep := Endpoints{
		OpenAIURL:    "https://api.openai.example",
		OpenAIKey:    "CONFIGURED-openai",
		AnthropicURL: "https://api.anthropic.example",
		AnthropicKey: "CONFIGURED-anthropic",
		GoogleURL:    "https://generativelanguage.example",
		GoogleKey:    "CONFIGURED-google",
		MistralURL:   "https://api.mistral.example",
		MistralKey:   "CONFIGURED-mistral",
	}
	const pooled = "POOLKEY-sentinel"

	authHeaders := func(cfg ProviderConfig) http.Header {
		req := httptest.NewRequest(http.MethodPost, "http://upstream/", nil)
		cfg.ApplyAuth(req)
		return req.Header
	}

	// 1 · openai → Authorization: Bearer <pooled> (the pooled key, NOT the configured one).
	t.Run("openai_Authorization_Bearer_pooled", func(t *testing.T) {
		got := authHeaders(ConfigForKey("openai", ep, pooled)).Get("Authorization")
		if got != "Bearer "+pooled {
			t.Fatalf("openai Authorization = %q, want %q", got, "Bearer "+pooled)
		}
		if strings.Contains(got, "CONFIGURED") {
			t.Fatalf("openai Authorization leaked the configured key: %q", got)
		}
	})

	// 2 · anthropic → x-api-key: <pooled> AND anthropic-version: 2023-06-01; no Authorization.
	t.Run("anthropic_xapikey_pooled_plus_version", func(t *testing.T) {
		h := authHeaders(ConfigForKey("anthropic", ep, pooled))
		if got := h.Get("x-api-key"); got != pooled {
			t.Fatalf("anthropic x-api-key = %q, want %q", got, pooled)
		}
		if got := h.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
		}
		if got := h.Get("Authorization"); got != "" {
			t.Fatalf("anthropic must not set Authorization, got %q", got)
		}
	})

	// 3 · google → the pooled key goes in the URL QUERY (key=<pooled>), not a header.
	t.Run("google_url_query_pooled_no_header", func(t *testing.T) {
		cfg := ConfigForKey("google", ep, pooled)
		gotURL := cfg.UpstreamURL("gemini-2.5-flash")
		if !strings.Contains(gotURL, "key="+pooled) {
			t.Fatalf("google URL = %q, want it to contain key=%s", gotURL, pooled)
		}
		if strings.Contains(gotURL, "CONFIGURED") {
			t.Fatalf("google URL leaked the configured key: %q", gotURL)
		}
		if got := authHeaders(cfg).Get("Authorization"); got != "" {
			t.Fatalf("google must set no Authorization header, got %q", got)
		}
	})

	// 4 · pass-through (mistral) → ConfigForKey leaves auth IDENTICAL to ConfigFor; the pooled key is NOT
	// applied (mistral/groq/vllm/bedrock are absent from the override switch), the configured key flows.
	t.Run("mistral_passthrough_pooled_NOT_applied", func(t *testing.T) {
		beforeAuth := authHeaders(ConfigFor("mistral", ep)).Get("Authorization")
		afterAuth := authHeaders(ConfigForKey("mistral", ep, pooled)).Get("Authorization")
		if afterAuth != beforeAuth {
			t.Fatalf("mistral auth changed by ConfigForKey: before=%q after=%q (pass-through expected)", beforeAuth, afterAuth)
		}
		if !strings.Contains(afterAuth, "CONFIGURED-mistral") {
			t.Fatalf("mistral must keep the configured key, got %q", afterAuth)
		}
		if strings.Contains(afterAuth, pooled) {
			t.Fatalf("mistral wrongly applied the pooled key: %q", afterAuth)
		}
	})
}
