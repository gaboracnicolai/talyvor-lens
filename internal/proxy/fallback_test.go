package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/retry"
	"github.com/talyvor/lens/internal/router"
)

// fastRetry produces a retry config that doesn't sleep at all so the
// fallback path is exercised quickly. Tests of fallback don't care
// about the exponential backoff curve — only about the eventual switch.
func fastRetry() retry.Config {
	return retry.Config{
		MaxAttempts:    1,
		BaseDelay:      0,
		MaxDelay:       0,
		RetryableCodes: []int{429, 500, 502, 503, 504},
	}
}

func newProxyWithFallback(t *testing.T, openAIURL, anthropicURL, googleURL string) *Proxy {
	t.Helper()
	exact, _ := newExactCacheForTest(t)
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		fallback.New(),
		"openai-key", "anthropic-key", "google-key",
	)
	p.openAIURL = openAIURL
	p.anthropicURL = anthropicURL
	p.googleURL = googleURL
	p.retryConfig = fastRetry()
	return p
}

func TestFallback_Primary500FallsBackToSecondary(t *testing.T) {
	var primaryHits, fallbackHits int
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryHits++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"upstream toast"}`)
	}))
	t.Cleanup(primary.Close)
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hello-from-anthropic"}}]}`)
	}))
	t.Cleanup(fallbackSrv.Close)

	p := newProxyWithFallback(t, primary.URL, fallbackSrv.URL, "")

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if primaryHits == 0 {
		t.Errorf("primary not hit")
	}
	if fallbackHits == 0 {
		t.Errorf("fallback not hit")
	}
	if got := w.Header().Get("X-Talyvor-Fallback-Provider"); got != "anthropic" {
		t.Errorf("X-Talyvor-Fallback-Provider = %q, want anthropic", got)
	}
	if got := w.Header().Get("X-Talyvor-Fallback-Model"); got != "claude-sonnet-4-6" {
		t.Errorf("X-Talyvor-Fallback-Model = %q, want claude-sonnet-4-6", got)
	}
}

func TestFallback_NetworkErrorFallsBack(t *testing.T) {
	// Closed server — connections are refused. forwardWithFallback must
	// treat that as a "should fall back" signal and try the next provider.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close() // address still resolves; connect will be refused

	var fallbackHits int
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(fallbackSrv.Close)

	p := newProxyWithFallback(t, dead.URL, fallbackSrv.URL, "")

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fallbackHits == 0 {
		t.Errorf("fallback not hit on network error")
	}
	if got := w.Header().Get("X-Talyvor-Fallback-Provider"); got != "anthropic" {
		t.Errorf("X-Talyvor-Fallback-Provider = %q, want anthropic", got)
	}
}

func TestFallback_AllProvidersFailReturnsLastError(t *testing.T) {
	mk500 := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"down"}`)
		}))
	}
	primary, second, third := mk500(), mk500(), mk500()
	t.Cleanup(primary.Close)
	t.Cleanup(second.Close)
	t.Cleanup(third.Close)

	p := newProxyWithFallback(t, primary.URL, second.URL, third.URL)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	// All three returned 500 — final response should surface a non-2xx
	// status. Either the upstream 500 propagates or the proxy synthesises
	// a 502; both prove fallback was exhausted.
	if w.Code == http.StatusOK {
		t.Fatalf("expected non-200 when every provider failed, got 200; body=%s", w.Body.String())
	}
}

func TestFallback_HeaderSetOnFallback(t *testing.T) {
	// Identical to the primary-500 test but asserts only the header presence.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(primary.Close)
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(fallbackSrv.Close)

	p := newProxyWithFallback(t, primary.URL, fallbackSrv.URL, "")

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	if w.Header().Get("X-Talyvor-Fallback-Provider") == "" {
		t.Error("X-Talyvor-Fallback-Provider must be set after a successful fallback")
	}
	if w.Header().Get("X-Talyvor-Fallback-Model") == "" {
		t.Error("X-Talyvor-Fallback-Model must be set after a successful fallback")
	}
}

func TestFallback_NoFallbackHeadersWhenPrimarySucceeds(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"primary-ok"}}]}`)
	}))
	t.Cleanup(primary.Close)

	p := newProxyWithFallback(t, primary.URL, "http://127.0.0.1:1", "")

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Talyvor-Fallback-Provider"); got != "" {
		t.Errorf("X-Talyvor-Fallback-Provider = %q on primary success, want empty", got)
	}
	if got := w.Header().Get("X-Talyvor-Fallback-Model"); got != "" {
		t.Errorf("X-Talyvor-Fallback-Model = %q on primary success, want empty", got)
	}
}
