package inference

import (
	"net/http"
	"net/url"
)

// Endpoints is the gateway's configured provider URLs + keys, passed BY VALUE into ConfigFor/ConfigForKey
// so a built ProviderConfig holds no pointer back into mutable proxy state (the bedrock snapshot semantics
// the PR-3b by-value test pinned). The proxy builds this from its own fields via p.endpoints(); the
// scorer builds it from config at startup.
type Endpoints struct {
	OpenAIURL    string
	OpenAIKey    string
	AnthropicURL string
	AnthropicKey string
	GoogleURL    string
	GoogleKey    string
	MistralURL   string
	MistralKey   string
	GroqURL      string
	GroqKey      string
	VLLMURL      string
	VLLMKey      string

	BedrockConfig BedrockConfig
	BedrockURL    string
}

// ConfigFor builds the per-provider ProviderConfig — moved VERBATIM from proxy.configForProvider (PR-3c),
// with the closures capturing the Endpoints VALUE instead of *Proxy fields. An unknown name yields a zero
// ProviderConfig (the nil-guarded methods then no-op), matching the proxy's prior default.
func ConfigFor(name string, ep Endpoints) ProviderConfig {
	switch name {
	case "openai":
		return ProviderConfig{
			name:          "openai",
			upstreamURLFn: func(string) string { return ep.OpenAIURL },
			setAuth: func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+ep.OpenAIKey)
			},
		}
	case "anthropic":
		return ProviderConfig{
			name:          "anthropic",
			upstreamURLFn: func(string) string { return ep.AnthropicURL },
			setAuth: func(req *http.Request) {
				req.Header.Set("x-api-key", ep.AnthropicKey)
				req.Header.Set("anthropic-version", "2023-06-01")
			},
		}
	case "google":
		return ProviderConfig{
			name: "google",
			upstreamURLFn: func(model string) string {
				return ep.GoogleURL + "/v1beta/models/" + model + ":generateContent?key=" + url.QueryEscape(ep.GoogleKey)
			},
			setAuth: func(*http.Request) {},
			translateRequest: func(body []byte) ([]byte, error) {
				out, _, err := TranslateToGemini(body)
				return out, err
			},
			translateResponse: TranslateFromGemini,
		}
	case "mistral":
		base := ep.MistralURL
		key := ep.MistralKey
		return ProviderConfig{
			name:          "mistral",
			upstreamURLFn: func(string) string { return base + "/v1/chat/completions" },
			setAuth: func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+key)
			},
		}
	case "groq":
		base := ep.GroqURL
		key := ep.GroqKey
		return ProviderConfig{
			name:          "groq",
			upstreamURLFn: func(string) string { return base + "/openai/v1/chat/completions" },
			setAuth: func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+key)
			},
		}
	case "vllm":
		base := ep.VLLMURL
		key := ep.VLLMKey
		return ProviderConfig{
			name:          "vllm",
			upstreamURLFn: func(string) string { return base + "/v1/chat/completions" },
			setAuth: func(req *http.Request) {
				// vLLM commonly runs without auth on private networks; only attach the header when an
				// operator-supplied key is configured.
				if key != "" {
					req.Header.Set("Authorization", "Bearer "+key)
				}
			},
		}
	case "bedrock":
		// Snapshot the bedrock state by value so the closures don't race with a concurrent config change.
		bedCfg := ep.BedrockConfig
		if bedCfg.Region == "" {
			bedCfg.Region = "us-east-1"
		}
		baseURL := ep.BedrockURL
		if baseURL == "" {
			baseURL = "https://bedrock-runtime." + bedCfg.Region + ".amazonaws.com"
		}
		return ProviderConfig{
			name: "bedrock",
			upstreamURLFn: func(model string) string {
				id, ok := ModelToBedrockID(model)
				if !ok {
					id = model
				}
				return baseURL + "/model/" + id + "/invoke"
			},
			setAuth: func(req *http.Request) {
				_ = SignRequest(req, bedCfg)
			},
			translateRequest:  TranslateToBedrockFormat,
			translateResponse: TranslateFromBedrockFormat,
		}
	}
	return ProviderConfig{}
}

// ConfigForKey returns ConfigFor(name, ep) with the auth (and, for google, the URL) closure rewritten to
// use the supplied POOLED key instead of the configured one — moved VERBATIM from proxy.applyKey (PR-3c).
// Only openai/anthropic/google are overridden; mistral/groq/vllm/bedrock pass through with their
// configured credentials unchanged. This reproduces the #258 applyKey credential map exactly.
func ConfigForKey(name string, ep Endpoints, key string) ProviderConfig {
	cfg := ConfigFor(name, ep)
	switch name {
	case "openai":
		cfg.setAuth = func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	case "anthropic":
		cfg.setAuth = func(req *http.Request) {
			req.Header.Set("x-api-key", key)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	case "google":
		base := ep.GoogleURL
		cfg.upstreamURLFn = func(model string) string {
			return base + "/v1beta/models/" + model + ":generateContent?key=" + url.QueryEscape(key)
		}
	}
	return cfg
}
