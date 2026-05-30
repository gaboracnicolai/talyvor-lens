package main

// providers.go — local-model provider adapters. The node binary
// uses these to talk to whatever inference server the operator
// has running (Ollama, vLLM, or llama.cpp). All three implement
// the same Provider interface so the inference HTTP handler
// stays provider-agnostic.

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

	"github.com/talyvor/lens/internal/povi"
)

// inferTimeout caps a single inference call. 60s matches the
// spec — model warm-up + first-token can take a while on cold
// GPUs but we don't want the network to wait forever.
const inferTimeout = 60 * time.Second

// healthTimeout is for the cheap "are you up" probe; should be
// near-instant on a running endpoint.
const healthTimeout = 3 * time.Second

// ─── interface ───────────────────────────────────

// Message is one chat turn — Provider.Infer turns this slice
// into whatever the provider's wire format expects.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// InferRequest is the provider-neutral inference call.
type InferRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens"`
	Stream    bool      `json:"stream"`
}

// InferResponse is what every adapter returns — text + actual
// (not estimated) token counts so the earnings record is honest.
//
// Receipt is the optional PoVI signed attestation (Token Economy Phase 1,
// Part 1): a node-signed commitment to this response's metadata + a Merkle
// root over the generation trace. It is ATTESTATION + TAMPER-EVIDENCE, not
// proof of honest computation. Present only when the node has a signing key.
type InferResponse struct {
	Text         string         `json:"text"`
	InputTokens  int            `json:"input_tokens"`
	OutputTokens int            `json:"output_tokens"`
	LatencyMs    int64          `json:"latency_ms"`
	Receipt      *povi.Receipt  `json:"receipt,omitempty"`
}

// Provider is the contract — implementations live below. Three
// methods cover the node lifecycle: probe (Health), enumerate
// (ListModels), and serve (Infer).
type Provider interface {
	Health(ctx context.Context) error
	ListModels(ctx context.Context) ([]string, error)
	Infer(ctx context.Context, req InferRequest) (InferResponse, error)
}

// ─── shared HTTP plumbing ────────────────────────

// providerBase carries the common fields every adapter has — a
// base URL + an HTTP client. We deliberately give each adapter
// its own client so per-provider timeouts are independent.
type providerBase struct {
	url    string
	client *http.Client
}

func newProviderBase(url string) providerBase {
	return providerBase{
		url:    strings.TrimRight(url, "/"),
		client: &http.Client{Timeout: inferTimeout},
	}
}

// doGet is the common GET helper — used by both health probes
// and model listings.
func (b *providerBase) doGet(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url+path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// doPost is the JSON-POST helper.
func (b *providerBase) doPost(ctx context.Context, path string, payload any) ([]byte, int, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url+path, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// estimateTokens is the fallback when the provider doesn't
// surface a token count (e.g. llama.cpp /completion). ~4 chars
// per token is the standard hand-wave.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len(s) / 4
	if n == 0 {
		n = 1
	}
	return n
}

// ─── Ollama ──────────────────────────────────────

// ollamaProvider talks to Ollama's REST API.
type ollamaProvider struct{ providerBase }

func NewOllamaProvider(url string) Provider {
	return &ollamaProvider{newProviderBase(url)}
}

func (p *ollamaProvider) Health(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	_, status, err := p.doGet(hctx, "/api/tags")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("ollama: /api/tags returned %d", status)
	}
	return nil
}

func (p *ollamaProvider) ListModels(ctx context.Context) ([]string, error) {
	body, status, err := p.doGet(ctx, "/api/tags")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("ollama: /api/tags returned %d", status)
	}
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("ollama: decode models: %w", err)
	}
	names := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

func (p *ollamaProvider) Infer(ctx context.Context, req InferRequest) (InferResponse, error) {
	payload := map[string]any{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   false,
	}
	if req.MaxTokens > 0 {
		payload["options"] = map[string]any{"num_predict": req.MaxTokens}
	}
	start := time.Now()
	body, status, err := p.doPost(ctx, "/api/chat", payload)
	if err != nil {
		return InferResponse{}, err
	}
	if status != http.StatusOK {
		return InferResponse{}, fmt.Errorf("ollama: chat returned %d: %s", status, body)
	}
	var resp struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount int `json:"prompt_eval_count"`
		EvalCount       int `json:"eval_count"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return InferResponse{}, fmt.Errorf("ollama: decode response: %w", err)
	}
	return InferResponse{
		Text:         resp.Message.Content,
		InputTokens:  resp.PromptEvalCount,
		OutputTokens: resp.EvalCount,
		LatencyMs:    time.Since(start).Milliseconds(),
	}, nil
}

// ─── vLLM ────────────────────────────────────────

// vllmProvider talks to vLLM's OpenAI-compatible API.
type vllmProvider struct{ providerBase }

func NewVLLMProvider(url string) Provider {
	return &vllmProvider{newProviderBase(url)}
}

func (p *vllmProvider) Health(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	_, status, err := p.doGet(hctx, "/v1/models")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("vllm: /v1/models returned %d", status)
	}
	return nil
}

func (p *vllmProvider) ListModels(ctx context.Context) ([]string, error) {
	body, status, err := p.doGet(ctx, "/v1/models")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("vllm: /v1/models returned %d", status)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("vllm: decode models: %w", err)
	}
	names := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		names = append(names, m.ID)
	}
	return names, nil
}

func (p *vllmProvider) Infer(ctx context.Context, req InferRequest) (InferResponse, error) {
	payload := map[string]any{
		"model":      req.Model,
		"messages":   req.Messages,
		"max_tokens": req.MaxTokens,
		"stream":     false,
	}
	start := time.Now()
	body, status, err := p.doPost(ctx, "/v1/chat/completions", payload)
	if err != nil {
		return InferResponse{}, err
	}
	if status != http.StatusOK {
		return InferResponse{}, fmt.Errorf("vllm: chat returned %d: %s", status, body)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return InferResponse{}, fmt.Errorf("vllm: decode response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return InferResponse{}, errors.New("vllm: empty choices in response")
	}
	return InferResponse{
		Text:         resp.Choices[0].Message.Content,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
		LatencyMs:    time.Since(start).Milliseconds(),
	}, nil
}

// ─── llama.cpp ───────────────────────────────────

// llamaCppProvider talks to the llama.cpp built-in server.
// Schema is simpler than the other two — single prompt string,
// no message turns, and the response carries enough usage info
// that we don't have to estimate.
type llamaCppProvider struct{ providerBase }

func NewLlamaCppProvider(url string) Provider {
	return &llamaCppProvider{newProviderBase(url)}
}

func (p *llamaCppProvider) Health(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	_, status, err := p.doGet(hctx, "/health")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("llamacpp: /health returned %d", status)
	}
	return nil
}

func (p *llamaCppProvider) ListModels(ctx context.Context) ([]string, error) {
	// llama.cpp serves a single model — fetch via /props.
	body, status, err := p.doGet(ctx, "/props")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("llamacpp: /props returned %d", status)
	}
	var out struct {
		ModelName string `json:"model_name"`
	}
	if err := json.Unmarshal(body, &out); err == nil && out.ModelName != "" {
		return []string{out.ModelName}, nil
	}
	return []string{"default"}, nil
}

func (p *llamaCppProvider) Infer(ctx context.Context, req InferRequest) (InferResponse, error) {
	// Flatten chat messages into a single prompt because the
	// /completion endpoint doesn't speak chat turns.
	var sb strings.Builder
	for _, m := range req.Messages {
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	prompt := sb.String()
	payload := map[string]any{
		"prompt":    prompt,
		"n_predict": req.MaxTokens,
		"stream":    false,
	}
	start := time.Now()
	body, status, err := p.doPost(ctx, "/completion", payload)
	if err != nil {
		return InferResponse{}, err
	}
	if status != http.StatusOK {
		return InferResponse{}, fmt.Errorf("llamacpp: /completion returned %d: %s", status, body)
	}
	var resp struct {
		Content   string `json:"content"`
		TokensPrompt int `json:"tokens_evaluated"`
		Tokens    int    `json:"tokens_predicted"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return InferResponse{}, fmt.Errorf("llamacpp: decode response: %w", err)
	}
	in := resp.TokensPrompt
	if in == 0 {
		in = estimateTokens(prompt)
	}
	out := resp.Tokens
	if out == 0 {
		out = estimateTokens(resp.Content)
	}
	return InferResponse{
		Text:         resp.Content,
		InputTokens:  in,
		OutputTokens: out,
		LatencyMs:    time.Since(start).Milliseconds(),
	}, nil
}

// ─── factory ─────────────────────────────────────

// NewProvider picks the right adapter for the configured
// provider string.
func NewProvider(kind, url string) (Provider, error) {
	switch kind {
	case "ollama":
		return NewOllamaProvider(url), nil
	case "vllm":
		return NewVLLMProvider(url), nil
	case "llamacpp":
		return NewLlamaCppProvider(url), nil
	}
	return nil, fmt.Errorf("unknown provider %q", kind)
}
