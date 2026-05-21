package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/cache"
)

func newExactCacheForTest(t *testing.T) (*cache.ExactCache, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)

	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	return cache.NewExactCache(rc, time.Minute), mr
}

func mockUpstream(t *testing.T, hits *int, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProxy_OpenAIExactCacheHit_LLMNeverCalled(t *testing.T) {
	var llmHits int
	upstream := mockUpstream(t, &llmHits, 200, `{"never":"called"}`)
	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, "openai-key", "anthropic-key")
	p.openAIURL = upstream.URL

	cached := []byte(`{"cached":"response"}`)
	if err := exact.Set(context.Background(), "openai", "gpt-4", "hello", cached); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != string(cached) {
		t.Fatalf("body = %q, want %q", w.Body.String(), cached)
	}
	if llmHits != 0 {
		t.Fatalf("LLM called %d times, expected 0", llmHits)
	}
}

func TestProxy_OpenAICacheMiss_ForwardsAndStores(t *testing.T) {
	var llmHits int
	const upstreamBody = `{"id":"chatcmpl-xyz","choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	upstream := mockUpstream(t, &llmHits, 200, upstreamBody)
	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, "openai-key", "anthropic-key")
	p.openAIURL = upstream.URL

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != upstreamBody {
		t.Fatalf("body = %q, want %q", w.Body.String(), upstreamBody)
	}
	if llmHits != 1 {
		t.Fatalf("LLM called %d times, expected 1", llmHits)
	}

	stored, err := exact.Get(context.Background(), "openai", "gpt-4", "hello")
	if err != nil {
		t.Fatalf("exact.Get after miss: %v", err)
	}
	if string(stored) != upstreamBody {
		t.Fatalf("cache stored %q, want %q", stored, upstreamBody)
	}
}

func TestProxy_AnthropicExactCacheHit_LLMNeverCalled(t *testing.T) {
	var llmHits int
	upstream := mockUpstream(t, &llmHits, 200, `{"never":"called"}`)
	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, "openai-key", "anthropic-key")
	p.anthropicURL = upstream.URL

	cached := []byte(`{"cached":"anthropic"}`)
	if err := exact.Set(context.Background(), "anthropic", "claude-3-opus-20240229", "hello", cached); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	body := `{"model":"claude-3-opus-20240229","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleAnthropic(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != string(cached) {
		t.Fatalf("body = %q, want %q", w.Body.String(), cached)
	}
	if llmHits != 0 {
		t.Fatalf("LLM called %d times, expected 0", llmHits)
	}
}

func TestProxy_AnthropicCacheMiss_ForwardsAndStores(t *testing.T) {
	var llmHits int
	var sawAPIKey, sawVersion string
	const upstreamBody = `{"id":"msg_1","content":[{"type":"text","text":"hi"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmHits++
		sawAPIKey = r.Header.Get("x-api-key")
		sawVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, "openai-key", "anthropic-key")
	p.anthropicURL = srv.URL

	body := `{"model":"claude-3-opus-20240229","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleAnthropic(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != upstreamBody {
		t.Fatalf("body = %q, want %q", w.Body.String(), upstreamBody)
	}
	if llmHits != 1 {
		t.Fatalf("LLM called %d times, expected 1", llmHits)
	}
	if sawAPIKey != "anthropic-key" {
		t.Errorf("x-api-key = %q, want %q", sawAPIKey, "anthropic-key")
	}
	if sawVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want %q", sawVersion, "2023-06-01")
	}

	stored, err := exact.Get(context.Background(), "anthropic", "claude-3-opus-20240229", "hello")
	if err != nil {
		t.Fatalf("exact.Get after miss: %v", err)
	}
	if string(stored) != upstreamBody {
		t.Fatalf("cache stored %q, want %q", stored, upstreamBody)
	}
}

func TestProxy_BodyTooLargeReturns413(t *testing.T) {
	var llmHits int
	upstream := mockUpstream(t, &llmHits, 200, `{}`)
	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, "openai-key", "anthropic-key")
	p.openAIURL = upstream.URL

	// 5 MiB body, larger than the 4 MiB cap
	big := strings.Repeat("a", 5<<20)
	req := httptest.NewRequest(http.MethodPost, "/v1/openai/v1/chat/completions", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", w.Code, w.Body.String())
	}
	if llmHits != 0 {
		t.Fatalf("LLM called %d times, expected 0", llmHits)
	}
}

func TestProxy_InvalidJSONReturns400(t *testing.T) {
	var llmHits int
	upstream := mockUpstream(t, &llmHits, 200, `{}`)
	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, "openai-key", "anthropic-key")
	p.openAIURL = upstream.URL

	req := httptest.NewRequest(http.MethodPost, "/v1/openai/v1/chat/completions", strings.NewReader(`{not valid json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if llmHits != 0 {
		t.Fatalf("LLM called %d times, expected 0", llmHits)
	}
}
