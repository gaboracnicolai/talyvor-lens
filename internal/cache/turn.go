package cache

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Turn is what the semantic cache compares for a chat request (B16.1): the LATEST user message,
// and a hash of every message before it.
//
// ⚠ THE SEMANTIC CACHE USED TO EMBED THE WHOLE CONVERSATION. Two chats that share their history
// and differ by one digit in the last question embed almost identically, so on 27 Sep Nicolai asked
// "how much is 2+2?" after "how much is 2+3?" → "2 + 3 = 5" and was served "It's still 5. 🙂" — the
// answer another of his chats got for "so how much is 2+3?". A Turn splits the request into what
// must be IDENTICAL for an answer to carry over (the history, hashed) and what may merely be THE
// SAME QUESTION (the latest message: embedded, entity-gated and pair-verified). A single-turn chat
// is the case with the empty history, which is how a rephrased first question still hits.
type Turn struct {
	// Latest is the text of the request's last message, which is a user message.
	Latest string
	// Prefix is the hex SHA-256 of every message before Latest, role and content. EmptyPrefix for a
	// single-turn request.
	Prefix string
	// History and AloneFP open B16.2's lane for a question asked mid-conversation: an answer stored
	// for the same question asked ON ITS OWN may serve it, if the verifier, shown History, confirms
	// the question stands alone. AloneFP is the request fingerprint the latest message would have
	// were it the whole conversation — what a standalone row was stored under. Both are empty for a
	// first question, and for a history the verifier cannot be shown faithfully (a system or tool
	// message, non-text content, more than maxHistoryChars).
	History string
	AloneFP string
}

// maxHistoryChars bounds the history shown to the verifier: a longer conversation is not offered
// B16.2's lane rather than cut, since the part cut could be the part the question depends on.
const maxHistoryChars = 4000

// MidConversation reports whether B16.2's lane is open for this request.
func (t Turn) MidConversation() bool { return t.Comparable() && t.AloneFP != "" }

// EmptyPrefix is the Prefix of a request with no history: a first question.
var EmptyPrefix = prefixHash(nil)

// Comparable reports whether the semantic cache may read or write for this request. The zero Turn
// is what LatestTurn returns for one it cannot compare; both semantic layers then stand aside and
// only the exact cache — byte-identical conversations — can serve it.
func (t Turn) Comparable() bool { return t.Prefix != "" }

// SingleTurn is the Turn of a conversation that is just this question.
func SingleTurn(question string) Turn { return Turn{Latest: question, Prefix: EmptyPrefix} }

// LatestTurn reads a chat body's Turn. Not comparable (the zero Turn) when the body is not a chat
// request, its last message is not from the user (an assistant prefill changes the answer), or
// that message is not plain text — an image or a document is not a question two texts can share.
func LatestTurn(body []byte) Turn {
	var req struct {
		Messages []chatMessage `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
		return Turn{}
	}
	last := req.Messages[len(req.Messages)-1]
	text, ok := messageText(last.Content)
	if last.Role != "user" || !ok || strings.TrimSpace(text) == "" {
		return Turn{}
	}
	t := Turn{Latest: text, Prefix: prefixHash(req.Messages[:len(req.Messages)-1])}
	if history, ok := renderHistory(req.Messages[:len(req.Messages)-1]); ok {
		t.History, t.AloneFP = history, aloneFingerprint(body)
	}
	return t
}

// renderHistory is the conversation before the latest question as the verifier reads it. Not ok for
// no history, a role other than user/assistant (a system prompt changes every answer after it), or
// content that is not text.
func renderHistory(msgs []chatMessage) (string, bool) {
	if len(msgs) == 0 {
		return "", false
	}
	var b strings.Builder
	for i, m := range msgs {
		text, ok := messageText(m.Content)
		if !ok {
			return "", false
		}
		switch m.Role {
		case "user":
			b.WriteString("User: ")
		case "assistant":
			b.WriteString("Assistant: ")
		default:
			return "", false
		}
		b.WriteString(text)
		if i < len(msgs)-1 {
			b.WriteString("\n\n")
		}
	}
	if b.Len() > maxHistoryChars {
		return "", false
	}
	return b.String(), true
}

// aloneFingerprint is RequestFingerprint of body with its messages cut to the last one: every
// setting kept, the history gone.
func aloneFingerprint(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if dec.Decode(&req) != nil {
		return ""
	}
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	req["messages"] = msgs[len(msgs)-1:]
	b, err := json.Marshal(req)
	if err != nil {
		return ""
	}
	return RequestFingerprint(b)
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// messageText is a message's text: a string, or content blocks that are ALL text, joined.
func messageText(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var blocks []struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "text" || b.Text == nil {
			return "", false
		}
		parts = append(parts, *b.Text)
	}
	return strings.Join(parts, "\n"), true
}

// prefixHash hashes the history canonically: each message's role and content, with the content
// re-encoded so JSON spacing and key order cannot make one history look like two. The rest of each
// message (name, tool calls) is already in the request fingerprint.
func prefixHash(msgs []chatMessage) string {
	canon := make([]any, 0, len(msgs))
	for _, m := range msgs {
		dec := json.NewDecoder(bytes.NewReader(m.Content))
		dec.UseNumber()
		var content any
		if err := dec.Decode(&content); err != nil {
			content = string(m.Content)
		}
		canon = append(canon, map[string]any{"role": m.Role, "content": content})
	}
	b, _ := json.Marshal(canon)
	return hexSHA256(b)
}
