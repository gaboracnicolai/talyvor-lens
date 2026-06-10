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

func TestTranslateToGemini_ConvertsMessagesCorrectly(t *testing.T) {
	body := []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hello"}]}`)

	out, model, err := translateToGemini(body)
	if err != nil {
		t.Fatalf("translateToGemini: %v", err)
	}
	if model != "gemini-2.5-pro" {
		t.Errorf("model = %q, want gemini-2.5-pro", model)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode out: %v", err)
	}
	contents, ok := got["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("contents shape wrong: %v", got["contents"])
	}
	c0 := contents[0].(map[string]any)
	if c0["role"] != "user" {
		t.Errorf("role = %v, want user", c0["role"])
	}
	parts, _ := c0["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("parts wrong: %v", parts)
	}
	if p0 := parts[0].(map[string]any); p0["text"] != "hello" {
		t.Errorf("text = %v, want hello", p0["text"])
	}
}

func TestTranslateToGemini_ExtractsSystemMessage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hi"}]}`)

	out, _, err := translateToGemini(body)
	if err != nil {
		t.Fatalf("translateToGemini: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)

	sys, ok := got["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction missing: %v", got)
	}
	sysParts := sys["parts"].([]any)
	if len(sysParts) != 1 || sysParts[0].(map[string]any)["text"] != "You are helpful" {
		t.Errorf("systemInstruction parts = %v", sysParts)
	}

	// Only the user message should remain in contents.
	contents := got["contents"].([]any)
	if len(contents) != 1 {
		t.Errorf("contents = %d entries, want 1 (system should not appear in contents)", len(contents))
	}
}

func TestTranslateToGemini_MapsAssistantToModel(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello back"}]}`)

	out, _, err := translateToGemini(body)
	if err != nil {
		t.Fatalf("translateToGemini: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)

	contents := got["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("contents = %d entries, want 2", len(contents))
	}
	if contents[0].(map[string]any)["role"] != "user" {
		t.Errorf("contents[0].role = %v, want user", contents[0].(map[string]any)["role"])
	}
	if contents[1].(map[string]any)["role"] != "model" {
		t.Errorf("contents[1].role = %v, want model (assistant should map to model)", contents[1].(map[string]any)["role"])
	}
}

func TestTranslateFromGemini_ConvertsResponseCorrectly(t *testing.T) {
	gemini := []byte(`{"candidates":[{"content":{"parts":[{"text":"hi from gemini"}],"role":"model"}}]}`)

	out, err := translateFromGemini(gemini, "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("translateFromGemini: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", got["object"])
	}
	if got["model"] != "gemini-2.5-flash" {
		t.Errorf("model = %v, want gemini-2.5-flash", got["model"])
	}
	choices := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(choices))
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Errorf("message.role = %v, want assistant", msg["role"])
	}
	if msg["content"] != "hi from gemini" {
		t.Errorf("message.content = %v, want %q", msg["content"], "hi from gemini")
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish_reason missing or wrong: %v", choices[0])
	}
}

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
