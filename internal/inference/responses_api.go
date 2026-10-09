package inference

import (
	"encoding/json"
	"errors"
	"strconv"
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
// with legacy functions, or a tool that is not a function, is refused rather than sent without it, since
// the answer would silently differ from what was asked.
//
// B17.104 — AND THEY TAKE CHAT'S TOOLS. Chat offers OpenAI models its function tools (Lens's wallet tools,
// Track, Docs), and a body carrying them was refused here — streamed, it went to /v1/responses untranslated
// and OpenAI answered 400 "Unsupported parameter: 'messages'", so all three models failed every question
// (e2e every-model, 2026-10-09). Function tools, tool_choice and parallel_tool_calls now go across; an
// assistant turn's tool_calls become function_call items and a tool turn a function_call_output, and the
// model's function_call items come back as chat tool_calls with finish_reason "tool_calls". The same run
// found GPT-5.6 and GPT-6 refusing function tools on /v1/chat/completions while they reason ("use
// /v1/responses or set reasoning_effort to 'none'"), so a body offering them tools goes there too
// (ServedByResponses).
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

// ServedByResponses reports whether an OpenAI chat body for model is sent to /v1/responses: always for a
// Responses-only model, and for GPT-5.6 and later (GPT-6 included) when the body offers tools and does
// not turn reasoning off — /v1/chat/completions refuses those models function tools while they reason.
func ServedByResponses(model string, body []byte) bool {
	if ResponsesOnly(model) {
		return true
	}
	if !toolsOnlyOnResponses(model) {
		return false
	}
	var c struct {
		Tools           json.RawMessage `json:"tools"`
		ReasoningEffort string          `json:"reasoning_effort"`
	}
	if json.Unmarshal(body, &c) != nil || c.ReasoningEffort == "none" {
		return false
	}
	t := strings.TrimSpace(string(c.Tools))
	return t != "" && t != "null" && t != "[]"
}

// toolsOnlyOnResponses reports whether model is GPT-5.6 or later: gpt-5.6-luna, gpt-6-sol, gpt-6.1.
func toolsOnlyOnResponses(model string) bool {
	rest, ok := strings.CutPrefix(model, "gpt-")
	if !ok {
		return false
	}
	version, _, _ := strings.Cut(rest, "-")
	majorS, minorS, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(majorS)
	if err != nil {
		return false
	}
	minor, _ := strconv.Atoi(minorS)
	return major > 5 || (major == 5 && minor >= 6)
}

// ResponsesURLFor turns .../v1/chat/completions into .../v1/responses, so an operator's base URL
// override still applies. A URL without that suffix has no known Responses endpoint.
func ResponsesURLFor(chatURL string) (string, bool) {
	if i := strings.LastIndex(chatURL, "/chat/completions"); i >= 0 {
		return chatURL[:i] + "/responses", true
	}
	return "", false
}

var ErrResponsesTools = errors.New("only function tools are carried to the Responses API")

// chatToolCall is one call in an assistant turn's tool_calls.
type chatToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToResponsesBody translates an OpenAI chat completions body into a Responses body.
func ToResponsesBody(body []byte) ([]byte, error) {
	var c struct {
		Model    string `json:"model"`
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCalls  []chatToolCall  `json:"tool_calls"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
		MaxTokens           *int   `json:"max_tokens"`
		MaxCompletionTokens *int   `json:"max_completion_tokens"`
		Stream              bool   `json:"stream"`
		ReasoningEffort     string `json:"reasoning_effort"`
		Tools               []struct {
			Type     string `json:"type"`
			Function *struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
				Strict      bool            `json:"strict"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice        json.RawMessage `json:"tool_choice"`
		ParallelToolCalls *bool           `json:"parallel_tool_calls"`
		Functions         json.RawMessage `json:"functions"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, err
	}
	if len(c.Functions) > 0 && string(c.Functions) != "null" {
		return nil, ErrResponsesTools
	}
	input := make([]map[string]any, 0, len(c.Messages))
	for _, m := range c.Messages {
		switch {
		case m.Role == "tool":
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": toolOutput(m.Content)})
		case len(m.ToolCalls) > 0:
			if said := strings.TrimSpace(string(m.Content)); said != "" && said != "null" && said != `""` && said != "[]" {
				input = append(input, map[string]any{"role": m.Role, "content": responsesContent(m.Role, m.Content)})
			}
			for _, tc := range m.ToolCalls {
				input = append(input, map[string]any{"type": "function_call", "call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments})
			}
		default:
			input = append(input, map[string]any{"role": m.Role, "content": responsesContent(m.Role, m.Content)})
		}
	}
	out := map[string]any{"model": c.Model, "input": input, "store": false}
	if len(c.Tools) > 0 {
		tools := make([]map[string]any, 0, len(c.Tools))
		for _, t := range c.Tools {
			if t.Type != "function" || t.Function == nil {
				return nil, ErrResponsesTools
			}
			// strict is sent as chat had it, false unless set: the Responses API's default is true, and a
			// strict tool whose schema leaves a property optional is refused.
			tool := map[string]any{"type": "function", "name": t.Function.Name, "strict": t.Function.Strict}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			if len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
				tool["parameters"] = t.Function.Parameters
			} else {
				tool["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, tool)
		}
		out["tools"] = tools
	}
	if tc := responsesToolChoice(c.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if c.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *c.ParallelToolCalls
	}
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

// responsesToolChoice carries chat's tool_choice: "auto", "none" and "required" as they are, and a named
// function ({"type":"function","function":{"name":…}}) as the Responses API names one.
func responsesToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var named struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &named) == nil && named.Type == "function" && named.Function != nil {
		return map[string]any{"type": "function", "name": named.Function.Name}
	}
	return raw
}

// toolOutput is a tool turn's answer as the function_call_output's text: a string as it is, text parts joined.
func toolOutput(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
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
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
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
	var calls []any
	for _, item := range r.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					text.WriteString(c.Text)
				}
			}
		case "function_call":
			calls = append(calls, map[string]any{"id": item.CallID, "type": "function",
				"function": map[string]any{"name": item.Name, "arguments": item.Arguments}})
		}
		// reasoning summaries are not part of the answer
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	finish := r.FinishReason()
	if len(calls) > 0 {
		message["tool_calls"] = calls
		if text.Len() == 0 {
			message["content"] = nil
		}
		if finish == "stop" {
			finish = "tool_calls"
		}
	}
	out := map[string]any{
		"id":      r.ID,
		"object":  "chat.completion",
		"created": r.CreatedAt,
		"model":   r.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
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
