package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/retry"
)

// ProviderInferer is the real, provider-backed implementation of the routing-prediction scorer's Inferer
// seam (routingscore.Inferer { Infer(ctx, model, input) (string, error) }). It resolves a model to its
// provider via the catalog (the package-level default registry, so operator catalog.LoadOverrides apply),
// builds the per-provider config with ConfigFor, runs the upstream round-trip with RunUpstream, and
// reverse-translates the response to OpenAI shape — reusing exactly the gateway's own provider machinery.
// It imports catalog + retry + stdlib only and NEVER internal/proxy (cycle-free); it satisfies
// routingscore.Inferer structurally (the interface check happens where main.go injects it, so this package
// stays independent of routingscore).
type ProviderInferer struct {
	httpClient *http.Client
	retry      retry.Config
	endpoints  Endpoints
}

// NewProviderInferer builds the Inferer from an HTTP client, a retry policy, and the gateway's provider
// Endpoints (passed by value — the bedrock snapshot stays a copy). Model→provider resolution uses the
// package-level catalog.Get (the same global registry, with overrides, that the gateway routes against).
func NewProviderInferer(httpClient *http.Client, rc retry.Config, ep Endpoints) *ProviderInferer {
	return &ProviderInferer{httpClient: httpClient, retry: rc, endpoints: ep}
}

// Infer runs `model` on `input` and returns the assistant's reply text. The flow mirrors the gateway's:
// resolve provider → ConfigFor → build (translate) request → RunUpstream → parse (reverse-translate) →
// extract the first choice's content.
func (pi *ProviderInferer) Infer(ctx context.Context, model, input string) (string, error) {
	m, ok := catalog.Get(model)
	if !ok {
		return "", fmt.Errorf("inferer: unknown model %q", model)
	}
	cfg := ConfigFor(m.Provider, pi.endpoints)

	body, err := buildOpenAIChatRequest(model, input)
	if err != nil {
		return "", err
	}
	sendBody, err := cfg.BuildRequest(body)
	if err != nil {
		return "", fmt.Errorf("inferer: build request: %w", err)
	}

	resp, respBody, _, err := RunUpstream(ctx, pi.httpClient, pi.retry, cfg.UpstreamURL(model), cfg.ApplyAuth, sendBody, nil)
	if err != nil {
		return "", fmt.Errorf("inferer: upstream call: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("inferer: upstream status %d for model %q", resp.StatusCode, model)
	}

	parsed, err := cfg.ParseResponse(respBody, model)
	if err != nil {
		return "", fmt.Errorf("inferer: parse response: %w", err)
	}
	return extractFirstChoiceContent(parsed)
}

// buildOpenAIChatRequest builds the canonical single-user-message chat-completions body. ConfigFor's
// per-provider BuildRequest then translates it to the provider's wire format (identity for OpenAI-shaped
// providers; gemini/bedrock translation otherwise).
func buildOpenAIChatRequest(model, input string) ([]byte, error) {
	out, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": input},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("inferer: marshal request: %w", err)
	}
	return out, nil
}

// extractFirstChoiceContent reads choices[0].message.content from an OpenAI-shape (post-ParseResponse)
// response body.
func extractFirstChoiceContent(respBody []byte) (string, error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return "", fmt.Errorf("inferer: decode response: %w", err)
	}
	if len(r.Choices) == 0 {
		return "", fmt.Errorf("inferer: response has no choices")
	}
	return r.Choices[0].Message.Content, nil
}
