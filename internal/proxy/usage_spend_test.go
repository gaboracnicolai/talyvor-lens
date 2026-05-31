package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// newSpendProxy builds a proxy whose upstream replies with the given body,
// wired to a recording alert sink + a logging-enabled workspace, so tests
// can assert exactly what gets billed.
func newSpendProxy(t *testing.T, upstreamBody string) (*Proxy, *recordingAlertSink) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "ws-log", Name: "spend-test", Active: true, LoggingPolicy: workspace.LoggingMetadata,
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
	return p, sink
}

// When the provider reports usage, spend is billed on the EXACT reported
// counts and the row is marked NOT estimated.
func TestSpend_NonStreamingOpenAIUsageBilledExact(t *testing.T) {
	p, sink := newSpendProxy(t, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":111,"completion_tokens":22,"total_tokens":133}}`)

	w := dispatchBody(t, p, "ws-log", `{"model":"gpt-4o","messages":[{"role":"user","content":"hello there"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sink.lastInput != 111 || sink.lastOutput != 22 {
		t.Fatalf("billed (%d,%d), want provider usage (111,22)", sink.lastInput, sink.lastOutput)
	}
	if sink.lastEstimated {
		t.Fatal("real provider usage must NOT be marked estimated")
	}
}

// Anthropic's native response shape (input_tokens/output_tokens) is billed
// exactly too — it has no translateResponse, so the body stays native.
func TestSpend_NonStreamingAnthropicUsageBilledExact(t *testing.T) {
	p, sink := newSpendProxy(t, `{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":77,"output_tokens":9}}`)

	body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sink.lastInput != 77 || sink.lastOutput != 9 {
		t.Fatalf("billed (%d,%d), want anthropic usage (77,9)", sink.lastInput, sink.lastOutput)
	}
	if sink.lastEstimated {
		t.Fatal("real anthropic usage must NOT be marked estimated")
	}
}

// Multimodal with real usage: the provider's prompt_tokens already folds in
// the image cost, so we bill that exact number — NOT the flat 1000-token
// ImageTokenEstimate — and the row is not estimated.
func TestSpend_NonStreamingMultimodalUsesProviderUsageNotFlat1000(t *testing.T) {
	p, sink := newSpendProxy(t, `{"choices":[{"message":{"role":"assistant","content":"a cat"}}],"usage":{"prompt_tokens":1500,"completion_tokens":4,"total_tokens":1504}}`)

	w := dispatchBody(t, p, "ws-log", imageBody("gpt-4o"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sink.lastInput != 1500 {
		t.Fatalf("multimodal input billed = %d, want provider's 1500 (NOT the flat estimate)", sink.lastInput)
	}
	if sink.lastEstimated {
		t.Fatal("multimodal with real usage must NOT be marked estimated")
	}
	if sink.lastModality != "image" {
		t.Fatalf("modality = %q, want image", sink.lastModality)
	}
}

// When usage is absent the path still bills (no regression), falls back to
// the len/4 estimate, and HONESTLY marks the row estimated.
func TestSpend_NonStreamingNoUsageFallsBackToEstimate(t *testing.T) {
	p, sink := newSpendProxy(t, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)

	w := dispatchBody(t, p, "ws-log", `{"model":"gpt-4o","messages":[{"role":"user","content":"hello there general"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sink.calls != 1 {
		t.Fatalf("RecordSpend calls = %d, want 1 (must still bill without usage)", sink.calls)
	}
	if !sink.lastEstimated {
		t.Fatal("absent usage must be marked estimated (honest fallback)")
	}
	// len("hello there general") = 19 → 19/4 = 4 (the existing estimate).
	if sink.lastInput != len("hello there general")/4 {
		t.Fatalf("fallback input = %d, want len/4 = %d", sink.lastInput, len("hello there general")/4)
	}
}
