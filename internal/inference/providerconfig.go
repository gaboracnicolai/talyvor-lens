package inference

import "net/http"

// ProviderConfig is the per-provider request/response seam — a small set of closures for BUILDING the
// upstream request (endpoint + auth + request translation) and PARSING/normalizing the response back to
// the canonical OpenAI shape. It moved here from internal/proxy (PR-3c) so both the gateway serve path and
// the routing-prediction scorer can build + drive a provider WITHOUT the serve handler. Fields stay
// UNEXPORTED — construction is exclusively through ConfigFor / ConfigForKey (config.go); proxy keeps a
// `type providerConfig = inference.ProviderConfig` alias for its type-name references.
type ProviderConfig struct {
	name              string
	upstreamURLFn     func(model string) string
	setAuth           func(*http.Request)
	translateRequest  func(body []byte) ([]byte, error)
	translateResponse func(body []byte, model string) ([]byte, error)
}

// Provider is the provider-plugin seam (Upgrade 16). It NAMES the contract ProviderConfig satisfies; the
// hot path still reads the config's closures directly. ProviderConfig is the single concrete impl, built
// per-request by ConfigFor.
type Provider interface {
	// ProviderName is the canonical provider name (matches catalog.Provider).
	ProviderName() string
	// UpstreamURL is the endpoint for a given model (some providers, e.g. Gemini, encode the model in
	// the path).
	UpstreamURL(model string) string
	// ApplyAuth stamps provider auth onto the outbound request.
	ApplyAuth(req *http.Request)
	// BuildRequest translates the canonical (OpenAI-shape) body to the provider's wire format. Identity
	// when the provider is OpenAI-compatible.
	BuildRequest(body []byte) ([]byte, error)
	// ParseResponse translates the provider's response back to OpenAI shape. Identity when the provider
	// is OpenAI-compatible.
	ParseResponse(body []byte, model string) ([]byte, error)
	// ExtractUsage reads the provider's reported token usage from a response body, returning ok=false
	// when no usage block is present (caller then estimates). See usage.go for the per-provider shapes.
	ExtractUsage(body []byte) (Usage, bool)
}

// The methods delegate to the closures, treating a nil closure as the identity/no-op so OpenAI-compatible
// providers (no translation) work unchanged.

func (c ProviderConfig) ProviderName() string { return c.name }

func (c ProviderConfig) UpstreamURL(model string) string {
	if c.upstreamURLFn == nil {
		return ""
	}
	return c.upstreamURLFn(model)
}

func (c ProviderConfig) ApplyAuth(req *http.Request) {
	if c.setAuth != nil {
		c.setAuth(req)
	}
}

func (c ProviderConfig) BuildRequest(body []byte) ([]byte, error) {
	if c.translateRequest == nil {
		return body, nil
	}
	return c.translateRequest(body)
}

func (c ProviderConfig) ParseResponse(body []byte, model string) ([]byte, error) {
	if c.translateResponse == nil {
		return body, nil
	}
	return c.translateResponse(body, model)
}

// ExtractUsage delegates to the name-dispatched free function (usage.go). A bare ExtractUsage here is the
// package-level func, not this method (methods are only reachable via a receiver).
func (c ProviderConfig) ExtractUsage(body []byte) (Usage, bool) {
	return ExtractUsage(c.name, body)
}

// Compile-time proof that ProviderConfig is a Provider.
var _ Provider = ProviderConfig{}
