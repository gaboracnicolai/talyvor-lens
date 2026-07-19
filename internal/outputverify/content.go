package outputverify

import (
	"encoding/json"
	"strings"
)

// content.go — the H5 CONTENT BINDING's byte definition. response_sha256 hashes the RAW served envelope (a
// JSON blob — identity, byte-frozen, never redefined); no buildable tree can ever contain it. THIS file
// defines the second, CONTENT-level hash: the canonical bytes of the assistant-authored text, exactly as the
// flagship writer (the talyvor-code agent's stripFences @ d6b8cc1) materializes them on disk. The commit
// endpoint folds THIS hash into the artifact output slot, so a committed manifest is satisfiable by a real,
// buildable tree whose slot file IS the generated code.
//
// THE PINNED DEFINITION (change it and every committed artifact stops matching — treat as frozen):
//
//	raw     = the assistant text of the served completion:
//	          anthropic → the concatenation of every content[i].text with content[i].type == "text",
//	                      in array order, no separators (tool_use/thinking blocks skipped);
//	          any other provider → choices[0].message.content, which must be a non-empty JSON string
//	                      (every non-anthropic provider is OpenAI-shaped at the capture site, native or
//	                      reverse-translated — the same dispatch rule as inference.ExtractUsage).
//	canonical(raw):
//	          1. s = strings.TrimSpace(raw)                        — outer whitespace only; interior
//	                                                                 bytes (incl. CRLF) are untouched
//	          2. if s starts with "```": drop through the first "\n" (the opening fence line)
//	          3. if the LAST "```" in s is s's suffix (nothing but that fence after it): drop it and
//	             any newlines immediately before it
//	          4. append "\n" unless s already ends with one       — exactly one trailing newline
//	ok=false (NOT COMMITTABLE → output_content_sha256 stays NULL) when: the body is not the provider's
//	non-streaming JSON envelope (true SSE streams are never captured; guardrail-buffered streams are
//	forced non-streaming upstream), no assistant text is present, or the canonical form is empty /
//	whitespace-only (nothing to bind).
//
// Steps 1–4 are byte-for-byte the flagship writer's stripFences (talyvor-code agent/cmd/agent/main.go @
// d6b8cc1) — including its quirks (prose after a closing fence survives; a blank line straight after the
// opening fence is preserved). That equality is the point: the hash must be over bytes BOTH sides reproduce
// identically, and the agent's on-disk file is the tree the attestor will manifest.
func CanonicalContent(provider string, responseBody []byte) (string, bool) {
	raw, ok := rawAssistantText(provider, responseBody)
	if !ok {
		return "", false
	}
	c := canonicalize(raw)
	if strings.TrimSpace(c) == "" {
		return "", false // empty / whitespace-only / fence-only — nothing to bind
	}
	return c, true
}

// CanonicalContentSHA256 is hex(sha256(CanonicalContent(...))) with the same ok semantics. This is the value
// captured into k4_output_verdicts.output_content_sha256 and folded into the artifact output slot.
func CanonicalContentSHA256(provider string, responseBody []byte) (string, bool) {
	c, ok := CanonicalContent(provider, responseBody)
	if !ok {
		return "", false
	}
	return Sha256Hex([]byte(c)), true
}

// rawAssistantText extracts the assistant text from the provider's non-streaming envelope. Dispatch mirrors
// inference.ExtractUsage: anthropic is native (content blocks); everything else is OpenAI-shaped.
func rawAssistantText(provider string, body []byte) (string, bool) {
	if provider == "anthropic" {
		var m struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(body, &m) != nil || len(m.Content) == 0 {
			return "", false
		}
		var b strings.Builder
		found := false
		for _, c := range m.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
				found = true
			}
		}
		if !found {
			return "", false
		}
		return b.String(), true
	}
	// OpenAI shape — reuses the exact semantics of the intrinsic verifier's responseText (verify.go):
	// choices[0].message.content, a non-empty string, or nothing.
	return responseText(body)
}

// canonicalize is steps 1–4 of the pinned definition — byte-for-byte the flagship writer's stripFences.
func canonicalize(s string) string {
	out := strings.TrimSpace(s)
	if strings.HasPrefix(out, "```") {
		if i := strings.Index(out, "\n"); i >= 0 {
			out = out[i+1:]
		}
	}
	if j := strings.LastIndex(out, "```"); j >= 0 && strings.TrimSpace(out[j:]) == "```" {
		out = strings.TrimRight(out[:j], "\n")
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}
