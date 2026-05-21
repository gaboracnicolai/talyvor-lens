package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
)

// flushRecorder is an httptest.ResponseRecorder that counts Flush() calls.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushCount int
}

func (f *flushRecorder) Flush() {
	f.flushCount++
	f.ResponseRecorder.Flush()
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
}

const openAISSEBody = "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n" +
	"data: [DONE]\n\n"

const anthropicSSEBody = "event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello \"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"world\"}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

func sseUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStream_OpenAIChunksForwardedToClient(t *testing.T) {
	srv := sseUpstream(t, openAISSEBody)
	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, compressor.New(), router.New(), pii.New(), nil, nil, nil, "openai-key", "anthropic-key")
	p.openAIURL = srv.URL

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := newFlushRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := w.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	out := w.Body.String()
	for _, want := range []string{"hello ", "world", "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("streamed body missing %q; got %q", want, out)
		}
	}
}

func TestStream_OpenAIFullResponseCachedAfterDone(t *testing.T) {
	srv := sseUpstream(t, openAISSEBody)
	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, compressor.New(), router.New(), pii.New(), nil, nil, nil, "openai-key", "anthropic-key")
	p.openAIURL = srv.URL

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := newFlushRecorder()

	p.HandleOpenAI(w, req)

	stored, err := exact.Get(context.Background(), "openai", "gpt-4", "hi")
	if err != nil {
		t.Fatalf("exact.Get: %v", err)
	}
	if stored == nil {
		t.Fatal("expected cached response after stream end, got nil")
	}
	if !strings.Contains(string(stored), "hello world") {
		t.Errorf("cached response = %q, want it to contain accumulated %q", stored, "hello world")
	}
}

func TestStream_AnthropicContentBlockDeltaAccumulated(t *testing.T) {
	srv := sseUpstream(t, anthropicSSEBody)
	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, compressor.New(), router.New(), pii.New(), nil, nil, nil, "openai-key", "anthropic-key")
	p.anthropicURL = srv.URL

	body := `{"model":"claude-3-opus-20240229","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := newFlushRecorder()

	p.HandleAnthropic(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	stored, err := exact.Get(context.Background(), "anthropic", "claude-3-opus-20240229", "hi")
	if err != nil {
		t.Fatalf("exact.Get: %v", err)
	}
	if stored == nil {
		t.Fatal("expected cached response after stream end, got nil")
	}
	if !strings.Contains(string(stored), "hello world") {
		t.Errorf("cached response = %q, want it to contain accumulated %q", stored, "hello world")
	}
}

func TestStream_NonStreamingRequestUsesExistingPath(t *testing.T) {
	var sawStreamHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawStreamHeader = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"non-stream"}}]}`)
	}))
	t.Cleanup(upstream.Close)

	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, compressor.New(), router.New(), pii.New(), nil, nil, nil, "openai-key", "anthropic-key")
	p.openAIURL = upstream.URL

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}` // no stream flag
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got == "text/event-stream" {
		t.Errorf("non-streaming request should not get SSE Content-Type; got %q", got)
	}
	if !strings.Contains(w.Body.String(), "non-stream") {
		t.Errorf("expected JSON body to pass through; got %q", w.Body.String())
	}
	_ = sawStreamHeader // upstream was hit at least once (recorded but not asserted)
}

func TestStream_FlushCalledAfterEachChunk(t *testing.T) {
	srv := sseUpstream(t, openAISSEBody)
	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, compressor.New(), router.New(), pii.New(), nil, nil, nil, "openai-key", "anthropic-key")
	p.openAIURL = srv.URL

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := newFlushRecorder()

	p.HandleOpenAI(w, req)

	// 2 data lines + at least one separator gets flushed before [DONE] break.
	if w.flushCount < 2 {
		t.Errorf("Flush called %d times, expected at least 2 (one per data chunk)", w.flushCount)
	}
}
