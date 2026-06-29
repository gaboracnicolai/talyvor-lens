package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
)

// NOTE (PR-3b A′): the translate UNIT tests (TestTranslateToGemini_*, TestTranslateFromGemini_*) moved to
// internal/inference with their funcs. The handler-level tests below stay here, byte-identical.

// geminiUpstream returns a mock that:
//   - rejects non-Gemini paths with 404,
//   - sends a fixed candidate text on /v1beta/models/<model>:generateContent.
func geminiUpstream(t *testing.T, hits *int32, sawURL *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ":generateContent") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		if sawURL != nil {
			*sawURL = r.URL.String()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"gemini says hello"}],"role":"model"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newGoogleProxy(t *testing.T, srvURL string) *Proxy {
	t.Helper()
	exact, _ := newExactCacheForTest(t)
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "google-key",
	)
	p.googleURL = srvURL
	return p
}

func TestHandleGoogle_ForwardsToCorrectURLWithAPIKey(t *testing.T) {
	var sawURL string
	srv := geminiUpstream(t, nil, &sawURL)

	p := newGoogleProxy(t, srv.URL)
	// Use an explicitly-cheap model so the router doesn't substitute it
	// out from under the URL assertion below.
	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/google/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.HandleGoogle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(sawURL, "/v1beta/models/gemini-2.5-flash:generateContent") {
		t.Errorf("upstream URL = %q, missing model path", sawURL)
	}
	if !strings.Contains(sawURL, "key=google-key") {
		t.Errorf("upstream URL = %q, missing API key", sawURL)
	}
}

func TestHandleGoogle_ReturnsOpenAIFormatResponse(t *testing.T) {
	srv := geminiUpstream(t, nil, nil)
	p := newGoogleProxy(t, srv.URL)

	body := `{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/google/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.HandleGoogle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, w.Body.String())
	}
	if got["object"] != "chat.completion" {
		t.Errorf("response.object = %v, want chat.completion (client should see OpenAI shape, not Gemini)", got["object"])
	}
	choices, _ := got["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in response: %v", got)
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if !strings.Contains(msg["content"].(string), "gemini says hello") {
		t.Errorf("content = %v, want gemini's text translated through", msg["content"])
	}
}

func TestHandleGoogle_CachesRepeatedRequests(t *testing.T) {
	var hits int32
	srv := geminiUpstream(t, &hits, nil)
	p := newGoogleProxy(t, srv.URL)

	send := func() *httptest.ResponseRecorder {
		body := `{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"same prompt"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/google/x", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		p.HandleGoogle(w, req)
		return w
	}

	w1 := send()
	if w1.Code != http.StatusOK {
		t.Fatalf("first call status = %d", w1.Code)
	}
	w2 := send()
	if w2.Code != http.StatusOK {
		t.Fatalf("second call status = %d", w2.Code)
	}
	if hits != 1 {
		t.Errorf("Gemini upstream hit %d times across two identical requests; want 1 (second should hit cache)", hits)
	}
}

func TestRouter_GoogleProviderRoutesToFlash(t *testing.T) {
	r := router.New()
	got := r.Route(context.Background(), "google", "gemini-2.5-pro", "hello")

	if got.Provider != "google" {
		t.Errorf("Provider = %q, want google", got.Provider)
	}
	if got.Model != "gemini-2.5-flash" {
		t.Errorf("Model = %q, want gemini-2.5-flash (cheap tier for google)", got.Model)
	}
	if got.CostTier != "cheap" {
		t.Errorf("CostTier = %q, want cheap", got.CostTier)
	}
}
