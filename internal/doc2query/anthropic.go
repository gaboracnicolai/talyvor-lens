package doc2query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// AnthropicDeriver derives questions with a cheap model.
//
// ⚠ MODEL CHOICE IS AN ECONOMIC ONE, and the margin is wide. Deriving costs roughly $0.0013 per
// stored answer on Haiku 4.5 ($1/$5 per 1M in/out): ~800 input tokens of answer plus ~100 output
// tokens of questions. A pooled hit avoids roughly $0.009 of Sonnet 5 generation. Break-even is
// therefore about 0.144 extra hits per stored answer — one extra hit for every seven answers
// stored. A frontier model would derive slightly better questions for ~10x the price and move
// break-even to ~1.4 hits per answer, which is a much harder bar.
type AnthropicDeriver struct {
	APIKey string
	Model  string
	HTTP   *http.Client
}

const anthropicMessagesURL = "https://api.anthropic.com/v1/messages"

func NewAnthropicDeriver(apiKey, model string) *AnthropicDeriver {
	if model == "" {
		model = "claude-haiku-4-5"
	}
	return &AnthropicDeriver{APIKey: apiKey, Model: model, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Derive returns up to n questions the answer would answer. It never returns the answer text.
func (d *AnthropicDeriver) Derive(ctx context.Context, answer string, n int) ([]string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      d.Model,
		"max_tokens": 512,
		"messages":   []map[string]string{{"role": "user", "content": fmt.Sprintf(Prompt, n, answer)}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", d.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doc2query: anthropic %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	var raw string
	for _, c := range out.Content {
		raw += c.Text
	}
	return ParseQuestions(raw, n), nil
}

// Answer generates a realistic stored answer for a question. Used only by the measurement harness:
// doc2query derives variants from ANSWERS, so measuring it without real answers would measure a
// different system.
func (d *AnthropicDeriver) Answer(ctx context.Context, question string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      d.Model,
		"max_tokens": 600,
		"messages":   []map[string]string{{"role": "user", "content": "Answer this technical question concisely and concretely, as a helpful assistant would. No preamble.\n\n" + question}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", d.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("doc2query: anthropic answer %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	var raw string
	for _, c := range out.Content {
		raw += c.Text
	}
	return raw, nil
}
