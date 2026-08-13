package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// ⚠⚠ WITH THE PROMPT REWRITER'S GATE CLOSED — WHICH IS EVERY WORKSPACE THAT EXISTS
// — THE PROVIDER STILL DOES NOT RECEIVE THE CALLER'S BYTES. EVERY ROLE IS DISCARDED
// AND THE WHOLE CONVERSATION ARRIVES AS ONE user MESSAGE.
//
// The compression block in proxy.go said the opposite: "With the gate closed
// compressedPrompt IS prompt, so rebuildBody below forwards the caller's bytes
// unchanged". Measured through the wire, red-first, on the smallest fixture that
// can tell the difference (a system message and a user message):
//
//	caller: [{"content":"You are a terse…","role":"system"},{"content":"What is 2+2?","role":"user"}]
//	sent:   [{"content":"You are a terse…\nWhat is 2+2?","role":"user"}]
//
// extractPrompt joins every message with "\n" and drops the roles; rebuildBody
// re-emits ONE user message holding that join. Both are long-standing and
// rebuildBody's own docstring states the collapse — the false sentence is the one
// the gate merge added, and it is corrected in the source. What NOTHING in this
// repo measured until now is the shape it is false on: every wire fixture in the
// compression family sends exactly ONE message, and with one message a collapse
// and a passthrough are the same bytes.
//
// ⚠ WHY IT IS NOT COSMETIC. A system instruction arrives as user text, and the
// model's OWN prior turn arrives attributed to the user. Providers weight those
// differently — that is what the role field is for.
//
// ⚠⚠ AND THE SAME CONVERSATION WITH "stream": true IS FORWARDED VERBATIM. The
// streaming branch returns before rebuildBody and sends the caller's body, so one
// flag decides whether the provider is asked the caller's question or a rewritten
// one. TestWire_StreamingForwardsTheRolesAndNonStreamingDoesNot is that
// differential in a single proxy, single workspace, single gate state.
//
// ⚠ PINNED AS MEASURED BEHAVIOUR, NOT FIXED, AND THE REASON IS A DECISION THIS
// SESSION WOULD NOT MAKE. rebuildBody's single-user-message body is the canonical
// shape every downstream stage is written against — the router analyses the join,
// cachePrompt IS the join, the spend estimate is len(join)/4, and the provider
// translators reverse-map from it. Preserving roles changes what is cached, what is
// billed and what each translator receives. WHAT THE DECISION IS: (a) carry the
// caller's messages array through and rewrite only the text of each message, or
// (b) keep the collapse and say so on the product surfaces that claim a
// pass-through gateway. These tests exist so (a) has to delete them deliberately.

// conversationUpstream captures every forwarded body and answers SSE or JSON
// depending on what the forwarded body asked for. ONE upstream for both halves of
// the differential, so "streamed" and "not streamed" cannot differ by wiring.
type conversationUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *conversationUpstream) add(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, b)
}

func (c *conversationUpstream) last(t *testing.T) []byte {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("upstream received no request at all — every assertion below would be vacuous")
	}
	return c.bodies[len(c.bodies)-1]
}

// messagesOf returns the "messages" array of a captured body, as decoded
// role/content pairs. The whole finding lives in this array's LENGTH.
func messagesOf(t *testing.T, body []byte) []struct{ Role, Content string } {
	t.Helper()
	var m struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, body)
	}
	out := make([]struct{ Role, Content string }, 0, len(m.Messages))
	for _, x := range m.Messages {
		out = append(out, struct{ Role, Content string }{x.Role, x.Content})
	}
	return out
}

// newConversationProxy wires one proxy whose upstream answers both shapes.
func newConversationProxy(t *testing.T, ws workspace.Workspace) (*Proxy, *conversationUpstream) {
	t.Helper()
	up := &conversationUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		up.add(b)
		if streamRequested(b) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, anthropicUsageSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	pointEveryProviderAt(p, srv.URL)
	p.setAlertSink(&recordingAlertSink{})
	return p, up
}

// theConversation is a four-turn exchange with THREE distinct roles. Every
// existing wire fixture in the compression family carries one message and one
// role, which is the shape in which a collapse is invisible.
func theConversation(stream bool) []byte {
	m := map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "system", "content": "You are a terse assistant. Never apologise."},
			{"role": "user", "content": "What is 2+2?"},
			{"role": "assistant", "content": "4"},
			{"role": "user", "content": "And 3+3?"},
		},
		"temperature": 0.2,
	}
	if stream {
		m["stream"] = true
	}
	b, _ := json.Marshal(m)
	return b
}

func dispatchConversation(t *testing.T, p *Proxy, wsID string, body []byte, stream bool) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", wsID)
	if stream {
		w := newFlushRecorder()
		p.HandleAnthropic(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("streamed status = %d, want 200", w.Code)
		}
		return
	}
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// THE PIN. Four messages in, one message out, and the roles are gone.
func TestWire_TheGateClosedPathDiscardsEveryRoleAndJoinsTheMessages(t *testing.T) {
	p, up := newConversationProxy(t, workspace.Workspace{
		ID: "ws-default", Name: "no policy set", Active: true,
	})
	caller := theConversation(false)

	// PREMISE, ASSERTED RATHER THAN ASSUMED: the fixture must carry more than one
	// message and more than one role, or a collapse is indistinguishable from a
	// pass-through and everything below passes for free.
	in := messagesOf(t, caller)
	if len(in) != 4 {
		t.Fatalf("premise: the fixture must carry 4 messages, got %d", len(in))
	}
	roles := map[string]bool{}
	for _, m := range in {
		roles[m.Role] = true
	}
	if len(roles) != 3 {
		t.Fatalf("premise: the fixture must carry 3 distinct roles, got %d (%v)", len(roles), roles)
	}

	dispatchConversation(t, p, "ws-default", caller, false)
	sent := messagesOf(t, up.last(t))

	if len(sent) != 1 {
		t.Fatalf("the collapse was fixed — the provider now receives %d messages. Re-measure this file "+
			"and delete it deliberately: %v", len(sent), sent)
	}
	if sent[0].Role != "user" {
		t.Errorf("the single surviving message must be a user one, which is the re-attribution; got %q", sent[0].Role)
	}
	const want = "You are a terse assistant. Never apologise.\nWhat is 2+2?\n4\nAnd 3+3?"
	if sent[0].Content != want {
		t.Errorf("expected every message newline-joined into one:\n want %q\n got  %q", want, sent[0].Content)
	}
	// NAMED SEPARATELY BECAUSE IT IS THE PART THAT CHANGES AN ANSWER: the system
	// instruction and the model's OWN prior turn both arrive as things the user said.
	if !strings.Contains(sent[0].Content, "Never apologise.") {
		t.Errorf("the system instruction should still be present, as user text: %q", sent[0].Content)
	}
	for _, m := range sent {
		if m.Role == "system" || m.Role == "assistant" {
			t.Errorf("a role survived the rebuild — the boundary moved: %v", sent)
		}
	}

	// AND THE SIBLING FIELDS DO SURVIVE, so this is a rebuild and not a refusal:
	// rebuildBody preserves everything except messages+model.
	var body map[string]any
	if err := json.Unmarshal(up.last(t), &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if body["temperature"] != 0.2 {
		t.Errorf("temperature should be forwarded untouched, got %v", body["temperature"])
	}
}

// THE DIFFERENTIAL, AND IT IS THE WHOLE FINDING IN ONE TEST. Same workspace, same
// closed gate, same four messages — only "stream" differs.
//
// ⚠ TWO PROXIES, AND THE REASON IS MEASURED RATHER THAN STYLISTIC: dispatching both
// halves against one proxy sends the SECOND one nowhere. The exact cache is keyed on
// wsID+prompt and the prompt is the same join either way, so the first answer is
// served back and up.last() silently returns the FIRST request's body — a
// differential that compares a body to itself and passes. That crossover is pinned
// in its own test below.
func TestWire_StreamingForwardsTheRolesAndNonStreamingDoesNot(t *testing.T) {
	ws := workspace.Workspace{ID: "ws-default", Name: "no policy set", Active: true}

	pStream, upStream := newConversationProxy(t, ws)
	dispatchConversation(t, pStream, "ws-default", theConversation(true), true)
	streamed := messagesOf(t, upStream.last(t))

	pBuf, upBuf := newConversationProxy(t, ws)
	dispatchConversation(t, pBuf, "ws-default", theConversation(false), false)
	buffered := messagesOf(t, upBuf.last(t))

	if len(streamed) != 4 {
		t.Fatalf("the streamed path must forward all four messages; got %d: %v", len(streamed), streamed)
	}
	if streamed[0].Role != "system" || streamed[2].Role != "assistant" {
		t.Errorf("the streamed path must forward the caller's roles verbatim; got %v", streamed)
	}
	if len(buffered) != 1 {
		t.Fatalf("the non-streamed path must collapse to one message; got %d: %v", len(buffered), buffered)
	}
	if len(streamed) == len(buffered) {
		t.Fatalf("the two paths agree now — the differential is gone and this test must be re-measured")
	}
}

// ⚠ AND THE TWO SHAPES SHARE ONE CACHE ENTRY. The streamed request is answered by a
// provider that saw four messages with their roles; the non-streamed request that
// follows is answered from that same entry, and its own path would have asked a
// different document. The key is wsID+the join, which is identical either way, so
// nothing in the key can tell the two apart.
func TestWire_AStreamedAnswerIsServedToTheNonStreamedRequestThatFollows(t *testing.T) {
	p, up := newConversationProxy(t, workspace.Workspace{
		ID: "ws-default", Name: "no policy set", Active: true,
	})

	dispatchConversation(t, p, "ws-default", theConversation(true), true)
	up.mu.Lock()
	afterStream := len(up.bodies)
	up.mu.Unlock()
	if afterStream != 1 {
		t.Fatalf("premise: the streamed request must reach the provider exactly once, got %d", afterStream)
	}

	dispatchConversation(t, p, "ws-default", theConversation(false), false)
	up.mu.Lock()
	afterBuffered := len(up.bodies)
	up.mu.Unlock()
	if afterBuffered != 1 {
		t.Fatalf("the non-streamed request reached the provider (%d calls) — the cache no longer "+
			"crosses the two shapes and this pin must be re-measured", afterBuffered)
	}
	// And the entry it was served IS the role-preserving one.
	if got := messagesOf(t, up.last(t)); len(got) != 4 {
		t.Errorf("the cached answer should be the one produced from the four-message body; got %v", got)
	}
}

// ⚠⚠ THE CONSEQUENCE FOR THE REWRITER, AND IT IS THE ONE THAT CORRUPTS CODE: because
// the rewriter is handed the JOIN, a ``` in the SYSTEM message pairs with the fence
// the USER wrote. fence_pairing_test.go measured the inversion inside one prompt;
// this measures the trigger arriving in a DIFFERENT MESSAGE — a system prompt is set
// once and reused, so one stray run there flattens code in every request after it,
// and the caller who sent the code cannot see the cause in their own message.
func TestWire_AStrayFenceInTheSystemMessageDestroysCodeInTheUserMessage(t *testing.T) {
	const userMsg = "Fix this:\n```python\ndef f():\n    if x:\n        return 1\n```\n"

	// CONTROL FIRST: the same user message under a system prompt with no ``` run.
	pOK, upOK := newConversationProxy(t, workspace.Workspace{
		ID: "ws-always", Name: "always", Active: true, CompressionPolicy: workspace.CompressionAlways,
	})
	okBody, _ := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "system", "content": "Wrap code in fences when you answer."},
			{"role": "user", "content": userMsg},
		},
	})
	dispatchConversation(t, pOK, "ws-always", okBody, false)
	protected := messagesOf(t, upOK.last(t))[0].Content
	if !strings.Contains(protected, "\n    if x:\n        return 1") {
		t.Fatalf("premise: with an even number of runs the user's block must survive verbatim; got %q", protected)
	}

	// THE ONLY DIFFERENCE IS THREE BACKTICKS IN THE SYSTEM MESSAGE.
	pBad, upBad := newConversationProxy(t, workspace.Workspace{
		ID: "ws-always", Name: "always", Active: true, CompressionPolicy: workspace.CompressionAlways,
	})
	badBody, _ := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "system", "content": "Wrap code in ``` fences when you answer."},
			{"role": "user", "content": userMsg},
		},
	})
	dispatchConversation(t, pBad, "ws-always", badBody, false)
	destroyed := messagesOf(t, upBad.last(t))[0].Content

	if strings.Contains(destroyed, "\n    if x:\n        return 1") {
		t.Errorf("the cross-message pairing was fixed — the user's block now survives a stray ``` "+
			"in the system message. That is the fix this test exists to force a deliberate deletion for: %q", destroyed)
	}
	if !strings.Contains(destroyed, "\n if x:\n return 1") {
		t.Errorf("expected both python nesting levels flattened to one space inside the user's own fence; got %q", destroyed)
	}
}
