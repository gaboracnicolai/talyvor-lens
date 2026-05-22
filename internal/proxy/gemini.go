package proxy

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// translateToGemini converts an OpenAI-compatible chat-completions body
// into Gemini's generateContent body, returning the new JSON, the model
// the caller requested (so the URL builder can substitute it into the
// path), and any decode error.
//
//   - "user" messages stay as "user" in Gemini.
//   - "assistant" messages map to "model".
//   - "system" messages are pulled out and packed into systemInstruction.
//   - Content blocks that are JSON arrays (rare in practice) get joined
//     into a single text string per part — Gemini's parts field is also
//     an array of typed objects, but our cache flow only ever produces
//     plain text content, so flattening is safe here.
func translateToGemini(body []byte) ([]byte, string, error) {
	var in struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, "", fmt.Errorf("gemini: decode openai body: %w", err)
	}

	type part struct {
		Text string `json:"text"`
	}
	type entry struct {
		Role  string `json:"role"`
		Parts []part `json:"parts"`
	}

	var contents []entry
	var systemParts []part
	for _, m := range in.Messages {
		text := contentToString(m.Content)
		switch m.Role {
		case "system":
			systemParts = append(systemParts, part{Text: text})
		case "assistant":
			contents = append(contents, entry{Role: "model", Parts: []part{{Text: text}}})
		default:
			// "user" and anything else falls through as user-role input.
			contents = append(contents, entry{Role: "user", Parts: []part{{Text: text}}})
		}
	}

	out := map[string]any{
		"contents": contents,
	}
	if len(systemParts) > 0 {
		out["systemInstruction"] = map[string]any{
			"parts": systemParts,
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, in.Model, fmt.Errorf("gemini: marshal gemini body: %w", err)
	}
	return encoded, in.Model, nil
}

// contentToString flattens a message content field into a plain string
// regardless of whether the value was a string or an array of typed
// content blocks (the same flattening extractPrompt uses).
func contentToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		out := ""
		for _, b := range blocks {
			out += b.Text
		}
		return out
	}
	return ""
}

// translateFromGemini converts a Gemini generateContent response into an
// OpenAI chat-completion shape so downstream callers (cache, client,
// scorer) treat Gemini responses identically to OpenAI/Anthropic ones.
func translateFromGemini(body []byte, model string) ([]byte, error) {
	var in struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
				Role string `json:"role"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("gemini: decode gemini response: %w", err)
	}
	if len(in.Candidates) == 0 {
		return nil, fmt.Errorf("gemini: response has no candidates")
	}

	// Join all parts of the first candidate into a single content string.
	text := ""
	for _, p := range in.Candidates[0].Content.Parts {
		text += p.Text
	}

	out := map[string]any{
		"id":     "gemini-" + uuid.NewString(),
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]any{{
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": "stop",
		}},
	}
	return json.Marshal(out)
}
