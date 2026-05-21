package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/metrics"
)

const (
	maxBodyBytes        = 4 << 20 // 4 MiB
	openAIChatURL       = "https://api.openai.com/v1/chat/completions"
	anthropicMessageURL = "https://api.anthropic.com/v1/messages"
	upstreamTimeout     = 120 * time.Second
)

type Proxy struct {
	exact        *cache.ExactCache
	semantic     *cache.SemanticCache
	embedder     cache.Embedder
	httpClient   *http.Client
	openAIKey    string
	anthropicKey string
	learner      *learner.Learner

	// Upstream URLs are unexported and defaulted so tests can swap them
	// for an httptest server without leaking config to callers.
	openAIURL    string
	anthropicURL string
}

// New constructs a Proxy. The learner is variadic so existing callers and
// tests that don't need usage analytics still compile; production wires
// a *learner.Learner as the last argument.
func New(
	exactCache *cache.ExactCache,
	semanticCache *cache.SemanticCache,
	embedder cache.Embedder,
	openAIKey string,
	anthropicKey string,
	learners ...*learner.Learner,
) *Proxy {
	p := &Proxy{
		exact:        exactCache,
		semantic:     semanticCache,
		embedder:     embedder,
		httpClient:   &http.Client{Timeout: upstreamTimeout},
		openAIKey:    openAIKey,
		anthropicKey: anthropicKey,
		openAIURL:    openAIChatURL,
		anthropicURL: anthropicMessageURL,
	}
	if len(learners) > 0 {
		p.learner = learners[0]
	}
	return p
}

// providerConfig holds the per-provider knobs HandleOpenAI/HandleAnthropic
// differ on. Everything else is shared in serve().
type providerConfig struct {
	name        string
	upstreamURL string
	setAuth     func(*http.Request)
}

func (p *Proxy) HandleOpenAI(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, providerConfig{
		name:        "openai",
		upstreamURL: p.openAIURL,
		setAuth: func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+p.openAIKey)
		},
	})
}

func (p *Proxy) HandleAnthropic(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, providerConfig{
		name:        "anthropic",
		upstreamURL: p.anthropicURL,
		setAuth: func(req *http.Request) {
			req.Header.Set("x-api-key", p.anthropicKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		},
	})
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, cfg providerConfig) {
	ctx := r.Context()

	body, err := readLimitedBody(r, maxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 4MB limit")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	model, prompt, err := extractPrompt(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	if cached := p.tryExact(ctx, cfg.name, model, prompt); cached != nil {
		writeBytes(w, http.StatusOK, cached)
		metrics.RequestsTotal.WithLabelValues(cfg.name, "cache_hit_exact").Inc()
		return
	}

	if cached := p.trySemantic(ctx, cfg.name, model, prompt); cached != nil {
		writeBytes(w, http.StatusOK, cached)
		metrics.RequestsTotal.WithLabelValues(cfg.name, "cache_hit_semantic").Inc()
		return
	}

	upstreamResp, upstreamBody, err := p.forward(ctx, r, body, cfg)
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(cfg.name, "error").Inc()
		writeError(w, http.StatusBadGateway, "upstream LLM error: "+err.Error())
		return
	}

	// Pass upstream status + content-type through to the client.
	if ct := upstreamResp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(upstreamResp.StatusCode)
	_, _ = w.Write(upstreamBody)

	if upstreamResp.StatusCode == http.StatusOK {
		p.storeCaches(ctx, cfg.name, model, prompt, upstreamBody)
		p.recordTokenEvent(ctx, cfg.name, model, prompt, upstreamBody)
		metrics.RequestsTotal.WithLabelValues(cfg.name, "forwarded").Inc()
	} else {
		metrics.RequestsTotal.WithLabelValues(cfg.name, "upstream_error").Inc()
	}
}

func (p *Proxy) recordTokenEvent(ctx context.Context, provider, model, prompt string, response []byte) {
	if p.learner == nil {
		return
	}
	// len/4 is the same token approximation the router and compressor use.
	_ = p.learner.Record(ctx, learner.TokenEvent{
		Provider:     provider,
		Model:        model,
		Prompt:       prompt,
		Response:     string(response),
		InputTokens:  len(prompt) / 4,
		OutputTokens: len(response) / 4,
		Cached:       false,
		Compressed:   false,
	})
}

func (p *Proxy) tryExact(ctx context.Context, provider, model, prompt string) []byte {
	if p.exact == nil {
		return nil
	}
	cached, err := p.exact.Get(ctx, provider, model, prompt)
	if err != nil || cached == nil {
		return nil
	}
	return cached
}

func (p *Proxy) trySemantic(ctx context.Context, provider, model, prompt string) []byte {
	if p.semantic == nil {
		return nil
	}
	cached, err := p.semantic.Get(ctx, provider, model, prompt)
	if err != nil || cached == nil {
		return nil
	}
	return cached
}

func (p *Proxy) storeCaches(ctx context.Context, provider, model, prompt string, response []byte) {
	if p.exact != nil {
		_ = p.exact.Set(ctx, provider, model, prompt, response)
	}
	if p.semantic != nil && p.embedder != nil {
		if vec, err := p.embedder.Embed(ctx, prompt); err == nil {
			_ = p.semantic.Set(ctx, provider, model, prompt, response, vec)
		}
	}
}

func (p *Proxy) forward(ctx context.Context, r *http.Request, body []byte, cfg providerConfig) (*http.Response, []byte, error) {
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build upstream request: %w", err)
	}

	for name, values := range r.Header {
		if strings.EqualFold(name, "Host") {
			continue
		}
		for _, v := range values {
			upstreamReq.Header.Add(name, v)
		}
	}
	cfg.setAuth(upstreamReq)

	resp, err := p.httpClient.Do(upstreamReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read upstream response: %w", err)
	}
	return resp, respBody, nil
}

func readLimitedBody(r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func extractPrompt(body []byte) (model, prompt string, err error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", "", err
	}

	var sb strings.Builder
	for i, m := range req.Messages {
		if i > 0 {
			sb.WriteByte('\n')
		}
		// Content is usually a string but may be an array of content blocks
		// (Anthropic). For caching purposes, string form is canonical; fall
		// back to raw JSON so block-form prompts still hash deterministically.
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			sb.WriteString(s)
		} else {
			sb.Write(m.Content)
		}
	}
	return req.Model, sb.String(), nil
}

func writeBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
