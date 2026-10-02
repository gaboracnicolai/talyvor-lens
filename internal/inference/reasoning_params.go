package inference

import (
	"encoding/json"
	"strings"
)

// AdaptReasoningParams rewrites a chat body bound for an OpenAI model that takes max_completion_tokens
// instead of max_tokens: every GPT-5.x and GPT-6 model, and chat-latest (B17.11 — the chat sends
// max_tokens: 4096, and each of them answered it with a 400). GPT-5.x and GPT-6 are reasoning models
// and reject temperature as well, so it is dropped for them; chat-latest takes it and keeps it. Sent
// as-is, the provider answers 400 and the user sees a dead reply. max_tokens moves to
// max_completion_tokens unless the caller already set that.
//
// Keyed on the DISPATCHED model, not the provider, so a request routed or fallen back onto one of
// these models is adapted too. Every other model's body, and any body that does not parse, passes
// through byte-for-byte. Called from the proxy's forward (buffered) and StreamHandler.serve (streamed) —
// the two copies of the upstream call — and from ProviderInferer.Infer (B26.10).
func AdaptReasoningParams(model string, body []byte) []byte {
	reasoning := strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "gpt-6")
	if !reasoning && model != "chat-latest" {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	maxTokens, hasMax := m["max_tokens"]
	_, hasTemp := m["temperature"]
	hasTemp = hasTemp && reasoning
	if !hasMax && !hasTemp {
		return body
	}
	if hasMax {
		if _, set := m["max_completion_tokens"]; !set {
			m["max_completion_tokens"] = maxTokens
		}
		delete(m, "max_tokens")
	}
	if hasTemp {
		delete(m, "temperature")
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
