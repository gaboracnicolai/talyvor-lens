package proxy

import (
	"net/http"
)

// ExtraProvider describes an OpenAI-compatible inference endpoint that
// Lens can proxy to. All three "extras" (vLLM, Mistral, Groq) use the
// OpenAI chat-completions wire format unchanged — they only differ in
// base URL and authentication mechanics. No request or response
// translation is needed; the proxy pipeline forwards bytes as-is.
type ExtraProvider struct {
	Name    string
	BaseURL string
	AuthFn  func(req *http.Request, apiKey string)
}

// ExtraProviders enumerates the OpenAI-compatible providers Lens
// supports outside the four "first-class" providers (OpenAI, Anthropic,
// Google, AWS Bedrock). The map drives both route registration in
// main.go and the readiness check in HandleExtraProvider.
var ExtraProviders = map[string]ExtraProvider{
	"vllm": {
		Name:    "vLLM",
		BaseURL: "", // No public default — the operator runs vLLM.
		AuthFn: func(req *http.Request, key string) {
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			// vLLM commonly runs auth-less on private networks; an
			// empty key is a valid deployment configuration.
		},
	},
	"mistral": {
		Name:    "Mistral",
		BaseURL: "https://api.mistral.ai",
		AuthFn: func(req *http.Request, key string) {
			req.Header.Set("Authorization", "Bearer "+key)
		},
	},
	"groq": {
		Name:    "Groq",
		BaseURL: "https://api.groq.com",
		AuthFn: func(req *http.Request, key string) {
			req.Header.Set("Authorization", "Bearer "+key)
		},
	},
}

// ExtraProviderConfig carries the runtime config Lens needs to talk to
// the three OpenAI-compatible providers. main.go populates this from
// the env-var-backed config; tests build one inline.
type ExtraProviderConfig struct {
	MistralKey string
	GroqKey    string
	VLLMURL    string
	VLLMKey    string
}

// SetExtraProviderConfig overlays Mistral / Groq / vLLM credentials
// onto the proxy. Called from main.go after proxy.New so the long
// constructor signature doesn't grow further. Empty fields are
// preserved as empty — HandleExtraProvider's readiness check uses
// presence to gate routes (no key → 503).
func (p *Proxy) SetExtraProviderConfig(cfg ExtraProviderConfig) {
	p.mistralKey = cfg.MistralKey
	p.groqKey = cfg.GroqKey
	p.vllmKey = cfg.VLLMKey
	if cfg.VLLMURL != "" {
		p.vllmURL = cfg.VLLMURL
	}
}

// extraProviderReady reports whether the given provider has enough
// configuration to serve a request. Mistral / Groq require an API key.
// vLLM requires a base URL (the API key is optional).
func (p *Proxy) extraProviderReady(provider string) bool {
	switch provider {
	case "mistral":
		return p.mistralKey != ""
	case "groq":
		return p.groqKey != ""
	case "vllm":
		return p.vllmURL != ""
	}
	return false
}

// HandleExtraProvider returns an http.HandlerFunc for one of the
// OpenAI-compatible providers in ExtraProviders. The closure validates
// readiness (returns 503 if not configured) and the provider name
// (returns 400 if unknown), then dispatches into the standard serve()
// pipeline via configForProvider. Cache + retry + fallback all work
// the same way as for the first-class providers; only the wire-format
// translation hooks are no-ops here.
func (p *Proxy) HandleExtraProvider(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ExtraProviders[provider]; !ok {
			writeError(w, http.StatusBadRequest, "unknown provider: "+provider)
			return
		}
		if !p.extraProviderReady(provider) {
			writeError(w, http.StatusServiceUnavailable, provider+" not configured")
			return
		}
		p.serve(w, r, p.configForProvider(provider))
	}
}
