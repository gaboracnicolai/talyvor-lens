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
// Usage is what one derivation actually cost, as the API reported it.
//
// ⚠ IT IS A RETURN VALUE, NOT A LOG LINE, and it exists because canonq/anthropic.go's own doc
// already said so about this file: "a client that discards the `usage` block the API already
// returned forces the next person to estimate it. doc2query's client discards it; this one does
// not." W2.6 then estimated its cost at ~50 tokens and measured 3.8x that. W2.7 asks for doc2query's
// cost per stored answer MEASURED, and an estimate is the one answer it rules out.
type Usage struct {
	InTokens  int
	OutTokens int
}

// Add accumulates another call's usage.
func (u *Usage) Add(v Usage) { u.InTokens += v.InTokens; u.OutTokens += v.OutTokens }

type AnthropicDeriver struct {
	APIKey string
	Model  string
	HTTP   *http.Client

	// BaseURL overrides the Anthropic endpoint. Empty means production. It exists so the usage
	// accounting can be asserted against a real HTTP round trip rather than a hand-built struct:
	// the field this code reads is a JSON tag, and a test that never decodes JSON cannot see a
	// typo in it.
	BaseURL string
}

func (d *AnthropicDeriver) url() string {
	if d.BaseURL != "" {
		return d.BaseURL
	}
	return anthropicMessagesURL
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
	qs, _, err := d.DeriveWithUsage(ctx, answer, n)
	return qs, err
}

// DeriveWithUsage is Derive plus what the call cost, as the API reported it.
func (d *AnthropicDeriver) DeriveWithUsage(ctx context.Context, answer string, n int) ([]string, Usage, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      d.Model,
		"max_tokens": 512,
		"messages":   []map[string]string{{"role": "user", "content": fmt.Sprintf(Prompt, n, answer)}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url(), bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", d.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, Usage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, Usage{}, fmt.Errorf("doc2query: anthropic %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, Usage{}, err
	}
	var raw string
	for _, c := range out.Content {
		raw += c.Text
	}
	return ParseQuestions(raw, n), Usage{InTokens: out.Usage.InputTokens, OutTokens: out.Usage.OutputTokens}, nil
}

// Answer generates a realistic stored answer for a question. Used only by the measurement harness:
// doc2query derives variants from ANSWERS, so measuring it without real answers would measure a
// different system.
func (d *AnthropicDeriver) Answer(ctx context.Context, question string) (string, error) {
	a, _, err := d.AnswerWithUsage(ctx, question)
	return a, err
}

// AnswerWithUsage is Answer plus what the call cost. The generation is HALF the write-time bill
// W2.7 asks to be measured — deriving questions from an answer presupposes an answer.
func (d *AnthropicDeriver) AnswerWithUsage(ctx context.Context, question string) (string, Usage, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      d.Model,
		"max_tokens": 600,
		"messages":   []map[string]string{{"role": "user", "content": "Answer this technical question concisely and concretely, as a helpful assistant would. No preamble.\n\n" + question}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url(), bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", d.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("doc2query: anthropic answer %d", resp.StatusCode)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", Usage{}, err
	}
	var raw string
	for _, c := range out.Content {
		raw += c.Text
	}
	return raw, Usage{InTokens: out.Usage.InputTokens, OutTokens: out.Usage.OutputTokens}, nil
}
