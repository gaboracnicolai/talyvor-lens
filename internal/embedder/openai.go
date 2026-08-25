package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const openAIEmbeddingsURL = "https://api.openai.com/v1/embeddings"

type OpenAIEmbedder struct {
	apiKey  string
	model   string
	client  *http.Client
	baseURL string
}

// NewOpenAIEmbedder builds an embedder for the OpenAI embeddings API. An empty
// baseURL uses the production endpoint (openAIEmbeddingsURL) — byte-identical to
// before; a non-empty value overrides it. The override is sourced ONLY from
// operator process env (LENS_EMBEDDING_BASE_URL, mirroring LENS_VLLM_BASE_URL):
// it lets the offline trial harness point embeddings at a deterministic mock. No
// request input (header, workspace config, body) can influence it.
func NewOpenAIEmbedder(apiKey, model, baseURL string) *OpenAIEmbedder {
	if baseURL == "" {
		baseURL = openAIEmbeddingsURL
	}
	return &OpenAIEmbedder{
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 30 * time.Second},
		baseURL: baseURL,
	}
}

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	// ⚠ THE API ALREADY COUNTS THE TOKENS AND THIS STRUCT USED TO THROW THEM AWAY. Every caller
	// that needed an embedding bill therefore had to estimate one, and W2.6 estimated ~50 tokens
	// where the truth was 3.8x that. Decoding the block costs nothing and removes the estimate.
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	v, _, err := e.EmbedWithUsage(ctx, text)
	return v, err
}

// EmbedWithUsage is Embed plus the token count the API billed, so a caller measuring write-time
// cost never has to estimate it. Embed delegates here: two request paths would be two places for
// the endpoint, the headers and the error handling to drift.
func (e *OpenAIEmbedder) EmbedWithUsage(ctx context.Context, text string) ([]float32, int, error) {
	payload, err := json.Marshal(embedRequest{Model: e.model, Input: text})
	if err != nil {
		return nil, 0, fmt.Errorf("openai embeddings: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("openai embeddings: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("openai embeddings: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("openai embeddings: status %d: %s", resp.StatusCode, body)
	}

	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, 0, fmt.Errorf("openai embeddings: decode response: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, 0, errors.New("openai embeddings: empty data in response")
	}
	return parsed.Data[0].Embedding, parsed.Usage.PromptTokens, nil
}

// Model reports the embedding model this embedder sends to the API — the same value used in
// every request body above. The semantic cache records it as the provenance of each stored
// vector, so it MUST be read from here rather than passed in separately: two copies of this
// name could drift apart, and a vector labelled with the wrong embedder is compared against
// vectors from a different space with no error anywhere (see migrations/0110).
func (e *OpenAIEmbedder) Model() string { return e.model }
