package proxy

import (
	"encoding/json"
	"strings"
)

// adaptReasoningParams rewrites a chat body bound for a GPT-6 model. GPT-6 models are reasoning
// models and reject two fields an ordinary chat client sends: max_tokens (they take
// max_completion_tokens) and temperature. Sent as-is, the provider answers 400 and the user sees a
// dead reply. max_tokens moves to max_completion_tokens unless the caller already set that, and
// temperature is dropped.
//
// Keyed on the DISPATCHED model, not the provider, so a request routed or fallen back onto a GPT-6
// model is adapted too. Every other model's body, and any body that does not parse, passes through
// byte-for-byte. Called from forward (buffered) and StreamHandler.serve (streamed) — the two copies
// of the upstream call.
func adaptReasoningParams(model string, body []byte) []byte {
	if !strings.HasPrefix(model, "gpt-6") {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	maxTokens, hasMax := m["max_tokens"]
	_, hasTemp := m["temperature"]
	if !hasMax && !hasTemp {
		return body
	}
	if hasMax {
		if _, set := m["max_completion_tokens"]; !set {
			m["max_completion_tokens"] = maxTokens
		}
		delete(m, "max_tokens")
	}
	delete(m, "temperature")
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
