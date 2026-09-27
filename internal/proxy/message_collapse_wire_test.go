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

// B18.6 — A MULTI-TURN CHAT REACHES THE MODEL WITH ITS TURNS INTACT, BUFFERED AND STREAMED.
//
// Until B18.6 the non-streamed path rebuilt every request as ONE user message holding every turn
// joined by "\n" (rebuildBody), whether or not anything was compressed: a system instruction and the
// model's own earlier answers reached the provider as things the user said, while the same
// conversation with "stream": true was forwarded verbatim. This file pinned that collapse as measured
// behaviour so a fix would have to delete the pin deliberately; B18.6 is that fix. Measured on the
// smallest fixture that can tell the difference — four messages, three roles.

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

// Four messages in, the same four out, in order, with their roles — and the sibling fields too.
func TestWire_TheBufferedPathForwardsEveryTurnAndRole(t *testing.T) {
	p, up := newConversationProxy(t, workspace.Workspace{
		ID: "ws-default", Name: "no policy set", Active: true,
	})
	caller := theConversation(false)

	// PREMISE, ASSERTED RATHER THAN ASSUMED: the fixture must carry more than one message and more
	// than one role, or a collapse is indistinguishable from a pass-through.
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
	if len(sent) != len(in) {
		t.Fatalf("the provider received %d messages, want the caller's %d: %v", len(sent), len(in), sent)
	}
	for i := range in {
		if sent[i] != in[i] {
			t.Errorf("message %d: sent %v, want the caller's %v", i, sent[i], in[i])
		}
	}
	var body map[string]any
	if err := json.Unmarshal(up.last(t), &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if body["temperature"] != 0.2 {
		t.Errorf("temperature should be forwarded untouched, got %v", body["temperature"])
	}
}

// Same workspace, same four messages, only "stream" differs — and the provider now receives the same
// turns either way.
//
// ⚠ TWO PROXIES: dispatching both halves against one proxy sends the SECOND one nowhere — the exact
// cache is keyed on wsID+prompt and the prompt is the same either way, so up.last() would return the
// FIRST request's body and compare it to itself.
func TestWire_StreamedAndBufferedForwardTheSameTurns(t *testing.T) {
	ws := workspace.Workspace{ID: "ws-default", Name: "no policy set", Active: true}

	pStream, upStream := newConversationProxy(t, ws)
	dispatchConversation(t, pStream, "ws-default", theConversation(true), true)
	streamed := messagesOf(t, upStream.last(t))

	pBuf, upBuf := newConversationProxy(t, ws)
	dispatchConversation(t, pBuf, "ws-default", theConversation(false), false)
	buffered := messagesOf(t, upBuf.last(t))

	if len(streamed) != 4 || len(buffered) != 4 {
		t.Fatalf("both paths must forward all four messages; streamed %v, buffered %v", streamed, buffered)
	}
	for i := range streamed {
		if streamed[i] != buffered[i] {
			t.Errorf("message %d differs by path: streamed %v, buffered %v", i, streamed[i], buffered[i])
		}
	}
}

// THE TWO SHAPES SHARE ONE CACHE ENTRY. The streamed request is answered by a provider that saw
// four messages with their roles; the non-streamed request that follows is answered from that same
// entry. Since B18.6 its own path would have sent the provider the same four messages, so sharing
// the entry is right; before, the two paths asked different documents under one key.
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
