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
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	payload, err := json.Marshal(embedRequest{Model: e.model, Input: text})
	if err != nil {
		return nil, fmt.Errorf("openai embeddings: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai embeddings: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai embeddings: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai embeddings: status %d: %s", resp.StatusCode, body)
	}

	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("openai embeddings: decode response: %w", err)
	}
	if len(parsed.Data) == 0 {
		return nil, errors.New("openai embeddings: empty data in response")
	}
	return parsed.Data[0].Embedding, nil
}
