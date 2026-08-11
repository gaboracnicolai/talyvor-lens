package canonq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Canonicaliser rewrites one prompt into a canonical question and reports what that cost.
//
// ⚠ USAGE IS PART OF THE RETURN VALUE, NOT A LOG LINE. W2.6 asks for a measured cost ratio rather
// than an estimate, and a client that discards the `usage` block the API already returned forces
// the next person to estimate it. doc2query's client discards it; this one does not.
type Canonicaliser interface {
	Canonicalise(ctx context.Context, prompt string) (Result, error)
}

// Result is one canonicalisation. Raw is kept beside Canonical because the interesting rows are
// the ones where Parse rejected the reply — "the model refused" and "the model answered the
// question instead" are different findings and both look like Canonical == "".
type Result struct {
	Canonical string
	Raw       string
	InTokens  int
	OutTokens int
}

// AnthropicCanonicaliser calls the cheap model. Model choice is the economic argument W2.6 asks
// to be measured, so it is a field rather than a constant.
type AnthropicCanonicaliser struct {
	APIKey string
	Model  string
	HTTP   *http.Client
}

const anthropicMessagesURL = "https://api.anthropic.com/v1/messages"

func NewAnthropicCanonicaliser(apiKey, model string) *AnthropicCanonicaliser {
	if model == "" {
		model = "claude-haiku-4-5"
	}
	return &AnthropicCanonicaliser{APIKey: apiKey, Model: model, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Canonicalise runs the fixed prompt at TEMPERATURE 0.
//
// ⚠ TEMPERATURE 0 IS NECESSARY AND NOT SUFFICIENT, AND THE ITEM SAYS SO: "MEASURE THE SAME INPUT
// TWICE — if one prompt yields two canonical forms, the key is unstable and the whole design
// collapses." Greedy decoding is not bit-reproducible across a served fleet; batching, kernel
// choice and hardware all move the argmax on near-ties. So temperature 0 is set here AND the
// harness re-runs every prompt to measure what it actually bought.
func (c *AnthropicCanonicaliser) Canonicalise(ctx context.Context, prompt string) (Result, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       c.Model,
		"max_tokens":  200,
		"temperature": 0,
		"messages":    []map[string]string{{"role": "user", "content": fmt.Sprintf(Prompt, prompt)}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("canonq: anthropic %d", resp.StatusCode)
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
		return Result{}, err
	}
	var raw string
	for _, ct := range out.Content {
		raw += ct.Text
	}
	return Result{
		Canonical: Parse(raw),
		Raw:       raw,
		InTokens:  out.Usage.InputTokens,
		OutTokens: out.Usage.OutputTokens,
	}, nil
}

// Answer generates a real answer for a question and reports its token cost. Used ONLY by the
// harness, to measure the denominator of the cost ratio W2.6 asks for against a real generation
// rather than the item's own "500-2000" placeholder.
func (c *AnthropicCanonicaliser) Answer(ctx context.Context, question string) (Result, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      c.Model,
		"max_tokens": 1024,
		"messages":   []map[string]string{{"role": "user", "content": "Answer this question concisely and concretely, as a helpful assistant would. No preamble.\n\n" + question}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicMessagesURL, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("canonq: anthropic answer %d", resp.StatusCode)
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
		return Result{}, err
	}
	var raw string
	for _, ct := range out.Content {
		raw += ct.Text
	}
	return Result{Raw: raw, InTokens: out.Usage.InputTokens, OutTokens: out.Usage.OutputTokens}, nil
}
