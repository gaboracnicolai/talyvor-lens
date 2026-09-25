package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// answerNeutralFields are the ONLY top-level request fields a cached answer may ignore. `model` is
// here because it is already its own key dimension; the rest cannot change what the model says.
//
// ⚠ THIS IS AN ALLOWLIST OF WHAT MAY BE DROPPED, NEVER A LIST OF WHAT MUST BE KEPT. Anthropic's
// top-level `system`, `tools`, `tool_choice`, `temperature`, `top_p`, `max_tokens`, `stop`,
// `response_format`, reasoning settings — and any field a provider adds tomorrow — are in the
// fingerprint because they are not named here. Before B15.1 the key was the messages' text alone,
// so two callers asking one question under different instructions shared one answer, within a
// workspace and across companies through the pool.
var answerNeutralFields = []string{"model", "stream", "stream_options", "metadata", "user", "safety_identifier"}

// RequestFingerprint is everything in a request that can change the answer except the prompt text:
// the body minus answerNeutralFields, with each message's `content` removed (the prompt carries it
// — the exact key hashes it, the semantic lookup embeds it) but its role, name and tool calls kept.
// Every cache layer requires an EQUAL fingerprint to serve: the exact and pooled keys hash it in
// (FingerprintedKey), and semantic rows store it in request_fp and are matched on it exactly, so a
// similar prompt can be served but never one asked under different settings.
//
// Numbers are compared as written (0.7 and 0.70 differ): a false miss costs one upstream call, a
// false hit serves a wrong answer.
func RequestFingerprint(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if err := dec.Decode(&req); err != nil || req == nil {
		// Not a JSON object: only a byte-identical body may share an entry.
		return hexSHA256(body)
	}
	for _, f := range answerNeutralFields {
		delete(req, f)
	}
	if msgs, ok := req["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				delete(mm, "content")
			}
		}
	}
	// json.Marshal sorts map keys at every depth, so field order in the request cannot matter.
	canon, err := json.Marshal(req)
	if err != nil {
		return hexSHA256(body)
	}
	return hexSHA256(canon)
}

// requestFingerprintMarker separates prompt key material from the fingerprint. The fingerprint is
// fixed-length hex at the END, so the joined string is injective whatever bytes the prompt holds.
const requestFingerprintMarker = "\x00req\x00"

// FingerprintedKey is the key material for an entry that answers prompt under fingerprint fp: the
// exact and pooled-exact keys are hashed from it, and so are semantic rows' prompt_hash, so entries
// for one prompt under different settings sit side by side instead of overwriting each other.
func FingerprintedKey(prompt, fp string) string { return prompt + requestFingerprintMarker + fp }

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
