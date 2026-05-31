package proxy

import (
	"context"
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

// captureUpstream is an SSE upstream that records the forwarded request body
// (so tests can assert stream_options injection) and replies with the given
// SSE script.
type captureUpstream struct {
	mu       sync.Mutex
	lastBody string
}

func newStreamSpendProxy(t *testing.T, sseBody string) (*Proxy, *recordingAlertSink, *captureUpstream) {
	t.Helper()
	cap := &captureUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.lastBody = string(raw)
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseBody)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "ws-log", Name: "stream-spend", Active: true, LoggingPolicy: workspace.LoggingMetadata,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	p.openAIURL = srv.URL
	p.anthropicURL = srv.URL
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)
	return p, sink, cap
}

func streamReqWS(t *testing.T, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	return req
}

const openAIUsageSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":8,\"total_tokens\":58}}\n\n" +
	"data: [DONE]\n\n"

const anthropicUsageSSE = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":40,\"output_tokens\":1}}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello \"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"world\"}}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

// THE HEADLINE FIX: a streamed OpenAI request now records spend (it used to
// be invisible to budgets/alerts), billed on the final-chunk usage, with
// stream_options.include_usage injected upstream so that usage is emitted.
func TestStreamSpend_OpenAIRecordsSpendWithRealUsage(t *testing.T) {
	p, sink, cap := newStreamSpendProxy(t, openAIUsageSSE)
	w := newFlushRecorder()
	p.HandleOpenAI(w, streamReqWS(t, "/v1/proxy/openai/v1/chat/completions",
		`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if sink.calls != 1 {
		t.Fatalf("RecordSpend called %d times on a stream, want 1 (streamed spend must NOT be invisible)", sink.calls)
	}
	if sink.lastInput != 50 || sink.lastOutput != 8 {
		t.Fatalf("billed (%d,%d), want final-chunk usage (50,8)", sink.lastInput, sink.lastOutput)
	}
	if sink.lastEstimated {
		t.Fatal("streamed spend on real usage must NOT be marked estimated")
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if !strings.Contains(cap.lastBody, `"include_usage":true`) {
		t.Fatalf("upstream stream request must inject stream_options.include_usage; body=%s", cap.lastBody)
	}
}

// Anthropic emits usage natively (message_start input + message_delta output,
// no flag). The streamed request records spend on those counts.
func TestStreamSpend_AnthropicRecordsSpendWithRealUsage(t *testing.T) {
	p, sink, cap := newStreamSpendProxy(t, anthropicUsageSSE)
	w := newFlushRecorder()
	p.HandleAnthropic(w, streamReqWS(t, "/v1/proxy/anthropic/v1/messages",
		`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}],"stream":true}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if sink.calls != 1 {
		t.Fatalf("RecordSpend called %d times on an Anthropic stream, want 1", sink.calls)
	}
	if sink.lastInput != 40 || sink.lastOutput != 7 {
		t.Fatalf("billed (%d,%d), want (input=40 from message_start, output=7 from message_delta)", sink.lastInput, sink.lastOutput)
	}
	if sink.lastEstimated {
		t.Fatal("streamed Anthropic spend on real usage must NOT be marked estimated")
	}
	// Anthropic gets NO include_usage (it emits usage natively).
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if strings.Contains(cap.lastBody, "include_usage") {
		t.Fatalf("include_usage must NOT be sent to Anthropic; body=%s", cap.lastBody)
	}
}

// Even when the stream emits NO usage, spend is STILL recorded (via the
// len/4 estimate) — a streamed request must never again be invisible.
func TestStreamSpend_NoUsageStillRecordsViaEstimate(t *testing.T) {
	p, sink, _ := newStreamSpendProxy(t, openAISSEBody) // openAISSEBody has no usage chunk
	w := newFlushRecorder()
	p.HandleOpenAI(w, streamReqWS(t, "/v1/proxy/openai/v1/chat/completions",
		`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if sink.calls != 1 {
		t.Fatalf("RecordSpend called %d times, want 1 (estimate fallback must still record)", sink.calls)
	}
	if !sink.lastEstimated {
		t.Fatal("streamed spend with no usage emitted must be marked estimated")
	}
}
