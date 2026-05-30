package proxy

import (
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/workspace"
)

// TestProviderSeam_ExistingProvidersDispatchIdentically is the
// behavior-preserving guard for the Upgrade 16 provider-plugin formalization:
// configForProvider still yields a Provider for each existing provider, with
// the same name, a usable endpoint, and OpenAI-compatible providers keeping
// identity request/response translation (no behavior change).
func TestProviderSeam_ExistingProvidersDispatchIdentically(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata) // we only need a constructed proxy

	openAICompatible := map[string]bool{"openai": true, "mistral": true, "groq": true, "vllm": true}

	for _, name := range []string{"openai", "anthropic", "google", "mistral", "groq", "vllm"} {
		var prov Provider = p.configForProvider(name)

		if prov.ProviderName() != name {
			t.Errorf("%s: ProviderName() = %q", name, prov.ProviderName())
		}
		if prov.UpstreamURL("gpt-4o") == "" {
			t.Errorf("%s: UpstreamURL must be non-empty", name)
		}
		// ApplyAuth must not panic (some providers set headers, vLLM may not).
		req := httptest.NewRequest("POST", "http://x/v1/chat/completions", nil)
		prov.ApplyAuth(req)

		// OpenAI-compatible providers translate identity; Anthropic/Google
		// have real translators (Anthropic via streaming/forward, Google here).
		body := []byte(`{"model":"x","messages":[]}`)
		if openAICompatible[name] {
			out, err := prov.BuildRequest(body)
			if err != nil || string(out) != string(body) {
				t.Errorf("%s: BuildRequest should be identity, got %q err %v", name, out, err)
			}
			rout, err := prov.ParseResponse(body, "x")
			if err != nil || string(rout) != string(body) {
				t.Errorf("%s: ParseResponse should be identity, got %q err %v", name, rout, err)
			}
		}
	}
}
