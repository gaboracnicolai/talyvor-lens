package main

// backend.go — backend adapters for the embedding-node binary.
// Two flavours: Ollama (POST /api/embeddings, one text at a
// time) and an OpenAI-compatible POST /v1/embeddings (batched).
//
// The HTTP server in server.go talks to one of these — choice
// is auto-detected at start-up by probing /api/tags first, then
// falling back to /v1/models if that 404s.

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
)

const backendTimeout = 60 * time.Second

// Backend is the contract both adapters implement.
type Backend interface {
	Name() string
	Embed(ctx context.Context, model string, texts []string) ([][]float64, error)
}

// ─── shared HTTP plumbing ────────────────────────

func newClient() *http.Client {
	return &http.Client{Timeout: backendTimeout}
}

func postJSON(ctx context.Context, client *http.Client, url string, payload any) ([]byte, int, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// ─── Ollama ──────────────────────────────────────

type ollamaBackend struct {
	url    string
	client *http.Client
}

// NewOllamaBackend wraps an Ollama embeddings endpoint. Ollama
// only accepts one text per call so we loop on the caller's
// behalf — slightly slower than batching but operationally
// simpler and Ollama tends to run colocated with the embedder
// anyway.
func NewOllamaBackend(url string) Backend {
	return &ollamaBackend{
		url:    strings.TrimRight(url, "/"),
		client: newClient(),
	}
}

func (b *ollamaBackend) Name() string { return "ollama" }

func (b *ollamaBackend) Embed(ctx context.Context, model string, texts []string) ([][]float64, error) {
	out := make([][]float64, 0, len(texts))
	for _, t := range texts {
		body, status, err := postJSON(ctx, b.client, b.url+"/api/embeddings",
			map[string]any{"model": model, "prompt": t})
		if err != nil {
			return nil, fmt.Errorf("ollama embed: %w", err)
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("ollama embed: status %d: %s", status, body)
		}
		var resp struct {
			Embedding []float64 `json:"embedding"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("ollama embed: decode: %w", err)
		}
		if len(resp.Embedding) == 0 {
			return nil, errors.New("ollama embed: empty embedding in response")
		}
		out = append(out, resp.Embedding)
	}
	return out, nil
}

// ─── OpenAI-compatible ───────────────────────────

// openAICompatBackend talks to anything that speaks the
// OpenAI /v1/embeddings shape (vLLM serving an embedding model,
// LocalAI, llama.cpp's embedding mode, etc).
type openAICompatBackend struct {
	url    string
	client *http.Client
}

func NewOpenAICompatBackend(url string) Backend {
	return &openAICompatBackend{
		url:    strings.TrimRight(url, "/"),
		client: newClient(),
	}
}

func (b *openAICompatBackend) Name() string { return "openai-compat" }

func (b *openAICompatBackend) Embed(ctx context.Context, model string, texts []string) ([][]float64, error) {
	body, status, err := postJSON(ctx, b.client, b.url+"/v1/embeddings",
		map[string]any{"model": model, "input": texts})
	if err != nil {
		return nil, fmt.Errorf("openai-compat embed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("openai-compat embed: status %d: %s", status, body)
	}
	var resp struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("openai-compat embed: decode: %w", err)
	}
	out := make([][]float64, 0, len(resp.Data))
	for _, d := range resp.Data {
		out = append(out, d.Embedding)
	}
	return out, nil
}

// ─── auto-detect ─────────────────────────────────

// DetectBackend probes the backend URL with both schemas and
// returns whichever responds with a 2xx first.
//   - GET /api/tags → ollama
//   - GET /v1/models → openai-compat
func DetectBackend(ctx context.Context, url string) (Backend, error) {
	client := newClient()
	url = strings.TrimRight(url, "/")
	if status := probe(ctx, client, url+"/api/tags"); status == http.StatusOK {
		return NewOllamaBackend(url), nil
	}
	if status := probe(ctx, client, url+"/v1/models"); status == http.StatusOK {
		return NewOpenAICompatBackend(url), nil
	}
	return nil, fmt.Errorf("backend at %s did not respond to /api/tags or /v1/models", url)
}

func probe(ctx context.Context, c *http.Client, url string) int {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
