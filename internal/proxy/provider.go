package proxy

import "net/http"

// Provider is the provider-plugin seam (Upgrade 16). Every upstream provider
// Lens supports is expressed as a providerConfig — a small set of closures
// for BUILDING the upstream request (endpoint + auth + request translation)
// and PARSING/normalizing the response back to the canonical OpenAI shape.
// The dispatch path (serve → forwardWithFallback) drives providers through
// this seam, so adding a provider is purely additive: add its models to the
// catalog and add one configForProvider case — no edits scattered across the
// proxy. See ADDING-A-PROVIDER.md.
//
// This interface NAMES the contract the existing providerConfig already
// satisfies; it does not change the dispatch flow (the hot path still reads
// the config's closures directly). providerConfig is the single concrete
// implementation, built per-request by configForProvider.
type Provider interface {
	// ProviderName is the canonical provider name (matches catalog.Provider).
	ProviderName() string
	// UpstreamURL is the endpoint for a given model (some providers, e.g.
	// Gemini, encode the model in the path).
	UpstreamURL(model string) string
	// ApplyAuth stamps provider auth onto the outbound request.
	ApplyAuth(req *http.Request)
	// BuildRequest translates the canonical (OpenAI-shape) body to the
	// provider's wire format. Identity when the provider is OpenAI-compatible.
	BuildRequest(body []byte) ([]byte, error)
	// ParseResponse translates the provider's response back to OpenAI shape.
	// Identity when the provider is OpenAI-compatible.
	ParseResponse(body []byte, model string) ([]byte, error)
}

// providerConfig satisfies Provider — the methods delegate to the closures,
// treating a nil closure as the identity/no-op so OpenAI-compatible providers
// (no translation) work unchanged.

func (c providerConfig) ProviderName() string { return c.name }

func (c providerConfig) UpstreamURL(model string) string {
	if c.upstreamURLFn == nil {
		return ""
	}
	return c.upstreamURLFn(model)
}

func (c providerConfig) ApplyAuth(req *http.Request) {
	if c.setAuth != nil {
		c.setAuth(req)
	}
}

func (c providerConfig) BuildRequest(body []byte) ([]byte, error) {
	if c.translateRequest == nil {
		return body, nil
	}
	return c.translateRequest(body)
}

func (c providerConfig) ParseResponse(body []byte, model string) ([]byte, error) {
	if c.translateResponse == nil {
		return body, nil
	}
	return c.translateResponse(body, model)
}

// Compile-time proof that the existing provider config is a Provider.
var _ Provider = providerConfig{}
