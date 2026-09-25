// Package pairverify is B9.2's pair verifier. Given a candidate pooled match — the question that
// was stored and the question being asked — it asks a small model whether the two have exactly the
// same correct answer, and allows the serve only on an unambiguous YES.
//
// ⚠ IT LOOKS AT THE PAIR, which is what every earlier mechanism could not do. Similarity,
// typographic folding, canonicalisation and doc2query each compared ONE question to a stored key,
// and the danger — direction and negation, "enable" vs "disable" — lives in the difference between
// two questions, not in either alone.
//
// ⚠ NOT WIRED INTO ANY SERVE PATH. B9.2 allows wiring only if the committed danger corpus is served
// ZERO times across three runs; cmd/pairverify is the measurement, docs/pool-b92-measured.md the
// result.
package pairverify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Prompt is the fixed instruction; the measurement is a property of this string as much as of the
// model, so it is committed beside the numbers it produced. %s, %s = the stored question, the asker's.
const Prompt = `A cached answer to Question A might be reused to answer Question B.

Question A: %s
Question B: %s

Would these two questions have exactly the same correct answer?
Any difference that could change the answer means NO: direction (enable/disable, to/from, increase/decrease), negation, a version, number, unit, date, place, person, product or other entity. Only a rewording of the same request means YES.

Reply with exactly one word: YES or NO.`

// Verdict is one check. Raw is kept because "the model said no" and "the model said something
// else" are different findings, and both refuse the serve.
type Verdict struct {
	Same      bool
	Raw       string
	InTokens  int
	OutTokens int
}

// Verifier checks one pair.
type Verifier interface {
	Verify(ctx context.Context, stored, asked string) (Verdict, error)
}

// Parse reads a reply. ONLY a bare YES (case, surrounding space and a final full stop ignored)
// allows a serve; "Yes, but…", an explanation, a refusal or an empty reply all refuse.
func Parse(raw string) bool {
	s := strings.TrimSuffix(strings.TrimSpace(raw), ".")
	return strings.EqualFold(s, "YES")
}

// AnthropicVerifier calls a small Anthropic model at temperature 0.
type AnthropicVerifier struct {
	APIKey string
	Model  string
	URL    string
	HTTP   *http.Client
}

// defaultModel is the cheap model the measurement ran on.
const defaultModel = "claude-haiku-4-5"

func NewAnthropicVerifier(apiKey, model string) *AnthropicVerifier {
	if model == "" {
		model = defaultModel
	}
	return &AnthropicVerifier{APIKey: apiKey, Model: model, URL: "https://api.anthropic.com/v1/messages",
		HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (v *AnthropicVerifier) Verify(ctx context.Context, stored, asked string) (Verdict, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       v.Model,
		"max_tokens":  5,
		"temperature": 0,
		"messages":    []map[string]string{{"role": "user", "content": fmt.Sprintf(Prompt, stored, asked)}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.URL, bytes.NewReader(body))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", v.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return Verdict{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Verdict{}, fmt.Errorf("pairverify: anthropic %d", resp.StatusCode)
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
		return Verdict{}, err
	}
	var raw string
	for _, c := range out.Content {
		raw += c.Text
	}
	return Verdict{Same: Parse(raw), Raw: raw, InTokens: out.Usage.InputTokens, OutTokens: out.Usage.OutputTokens}, nil
}
