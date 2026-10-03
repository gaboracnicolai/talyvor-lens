package inference

import (
	"encoding/json"
	"errors"
	"strings"
)

// B17.11 — OPENAI'S PRO AND CODEX MODELS ANSWER THE CHAT.
//
// GPT-5.3 Codex, GPT-5.4 Pro and GPT-5.5 Pro are served only by POST /v1/responses: asked on
// /v1/chat/completions, OpenAI answers 404 ("not supported in the v1/chat/completions endpoint"), so the
// picker offered three models that could never reply. Lens keeps its OpenAI chat shape on both sides —
// the client sends and receives chat completions, billing reads chat usage — and translates only the
// upstream leg: the chat body becomes a Responses body, and the Responses reply (buffered) or event
// stream (streamed) becomes a chat completion or chat.completion.chunk events.
//
// Carried: the messages (text and image parts), the output cap (max_completion_tokens, else max_tokens)
// and reasoning_effort. Not carried: temperature and top_p, which these reasoning models reject. A body
// with tools or functions is refused rather than sent without them, since the answer would silently
// differ from what was asked.
//
// The buffered translation lives here so the proxy's forward and ProviderInferer.Infer share it (B26.10);
// the streamed leg is the proxy's (internal/proxy/responses_api.go).

// ResponsesOnly reports whether an OpenAI model is served only by /v1/responses: the -pro and -codex
// models, dated snapshots included (gpt-5.4-pro-2026-03-05).
func ResponsesOnly(model string) bool {
	if !strings.HasPrefix(model, "gpt-") {
		return false
	}
	for _, part := range strings.Split(model, "-") {
		if part == "pro" || part == "codex" {
			return true
		}
	}
	return false
}

// ResponsesURLFor turns .../v1/chat/completions into .../v1/responses, so an operator's base URL
// override still applies. A URL without that suffix has no known Responses endpoint.
func ResponsesURLFor(chatURL string) (string, bool) {
	if i := strings.LastIndex(chatURL, "/chat/completions"); i >= 0 {
		return chatURL[:i] + "/responses", true
	}
	return "", false
}

var ErrResponsesTools = errors.New("tools are not carried to the Responses API")

// ToResponsesBody translates an OpenAI chat completions body into a Responses body.
func ToResponsesBody(body []byte) ([]byte, error) {
	var c struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens           *int            `json:"max_tokens"`
		MaxCompletionTokens *int            `json:"max_completion_tokens"`
		Stream              bool            `json:"stream"`
		ReasoningEffort     string          `json:"reasoning_effort"`
		Tools               json.RawMessage `json:"tools"`
		Functions           json.RawMessage `json:"functions"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, err
	}
	if (len(c.Tools) > 0 && string(c.Tools) != "null") || (len(c.Functions) > 0 && string(c.Functions) != "null") {
		return nil, ErrResponsesTools
	}
	input := make([]map[string]any, 0, len(c.Messages))
	for _, m := range c.Messages {
		input = append(input, map[string]any{"role": m.Role, "content": responsesContent(m.Role, m.Content)})
	}
	out := map[string]any{"model": c.Model, "input": input, "store": false}
	if c.Stream {
		out["stream"] = true
	}
	if c.MaxCompletionTokens != nil {
		out["max_output_tokens"] = *c.MaxCompletionTokens
	} else if c.MaxTokens != nil {
		out["max_output_tokens"] = *c.MaxTokens
	}
	if c.ReasoningEffort != "" {
		out["reasoning"] = map[string]any{"effort": c.ReasoningEffort}
	}
	return json.Marshal(out)
}

// responsesContent carries one chat message's content: a string as it is, content parts retyped —
// text to input_text (output_text for the assistant's own turns), image_url to input_image.
func responsesContent(role string, raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return raw
	}
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p["type"] {
		case "text":
			out = append(out, map[string]any{"type": textType, "text": p["text"]})
		case "image_url":
			img := map[string]any{"type": "input_image"}
			switch u := p["image_url"].(type) {
			case string:
				img["image_url"] = u
			case map[string]any:
				img["image_url"] = u["url"]
				if d, ok := u["detail"]; ok {
					img["detail"] = d
				}
			}
			out = append(out, img)
		default:
			out = append(out, p)
		}
	}
	return out
}

// ResponsesUsage is a Responses usage block.
type ResponsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u ResponsesUsage) Cached() int {
	if u.InputTokensDetails == nil {
		return 0
	}
	return u.InputTokensDetails.CachedTokens
}

// ResponsesReply is a Responses object: the buffered reply, and the `response` of a stream's last event.
type ResponsesReply struct {
	ID                string `json:"id"`
	CreatedAt         int64  `json:"created_at"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *ResponsesUsage `json:"usage"`
}

// finishReason is the chat finish_reason for a finished Responses object.
func (r ResponsesReply) FinishReason() string {
	if r.Status != "incomplete" {
		return "stop"
	}
	if r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "content_filter" {
		return "content_filter"
	}
	return "length"
}

// FromResponsesBody translates a buffered Responses reply into a chat completion, usage included, so
// everything after the upstream call — the client, the cache, the charge — reads the shape it always has.
func FromResponsesBody(raw []byte) ([]byte, error) {
	var r ResponsesReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	var text strings.Builder
	for _, item := range r.Output {
		if item.Type != "message" {
			continue // reasoning summaries are not part of the answer
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				text.WriteString(c.Text)
			}
		}
	}
	out := map[string]any{
		"id":      r.ID,
		"object":  "chat.completion",
		"created": r.CreatedAt,
		"model":   r.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text.String()},
			"finish_reason": r.FinishReason(),
		}},
	}
	if u := r.Usage; u != nil {
		usage := map[string]any{"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens,
			"total_tokens": u.InputTokens + u.OutputTokens}
		if c := u.Cached(); c > 0 {
			usage["prompt_tokens_details"] = map[string]any{"cached_tokens": c}
		}
		if d := u.OutputTokensDetails; d != nil && d.ReasoningTokens > 0 {
			usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": d.ReasoningTokens}
		}
		out["usage"] = usage
	}
	return json.Marshal(out)
}
