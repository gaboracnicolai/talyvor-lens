package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/workspace"
)

// dispatchRouting posts a chat request with an explicit `model` and a `prompt`,
// optionally setting the X-Talyvor-Auto-Route delegation header. Returns the
// recorder so a test can assert the SERVED model (via the sink) and headers.
func dispatchRouting(t *testing.T, p *Proxy, wsID, model, prompt string, autoHeader bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"` + prompt + `"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", wsID)
	if autoHeader {
		req.Header.Set("X-Talyvor-Auto-Route", "true")
	}
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	return w
}

// servedModel returns the model the upstream spend row recorded — the model that
// ACTUALLY served the request (upstreamModel), not the one the caller named.
func servedModel(t *testing.T, sink *recordingAlertSink) string {
	t.Helper()
	s, ok := sink.spendWithServeSource("")
	if !ok {
		t.Fatal("no upstream spend recorded — the request did not serve")
	}
	return s.model
}

const simplePrompt = "What is the capital of France?" // AnalyseComplexity score 0 → cheap tier

// THE FOUNDER'S RULE: an explicitly named model is served by that model. gpt-4o
// on a trivially simple prompt (which the substring heuristic scores as cheap)
// must NOT be silently downgraded to gpt-4o-mini — the customer chose gpt-4o.
func TestRoutingConsent_ConcreteModelHonoured(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)

	w := dispatchRouting(t, p, "ws-log", "gpt-4o", simplePrompt, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := servedModel(t, sink); got != "gpt-4o" {
		t.Fatalf("an explicit gpt-4o request MUST be served by gpt-4o (honoured), got %q", got)
	}
	if h := w.Header().Get("X-Talyvor-Routed"); h != "" {
		t.Fatalf("a pinned model must not be routed or announce a substitution; header=%q", h)
	}
}

// The X-Talyvor-Auto-Route header is per-request DELEGATION: the customer ceded
// the choice, so routing may downgrade a concrete model — and MUST announce it.
func TestRoutingConsent_AutoRouteHeaderRoutesAndAnnounces(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)

	w := dispatchRouting(t, p, "ws-log", "gpt-4o", simplePrompt, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := servedModel(t, sink); got != "gpt-4o-mini" {
		t.Fatalf("a delegated (auto-route header) request should route to the cheap model, got %q", got)
	}
	if h := w.Header().Get("X-Talyvor-Routed"); h != "gpt-4o→gpt-4o-mini" {
		t.Fatalf("a delegated routing substitution must be announced; header=%q", h)
	}
}

// A per-workspace opt-in ("optimise my costs, you pick the model") is consented
// delegation: routing may downgrade a concrete model, and MUST announce it.
func TestRoutingConsent_WorkspaceOptInRoutesAndAnnounces(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	if err := p.workspaceManager.SetCostOptimizeRouting(context.Background(), "ws-log", true); err != nil {
		t.Fatalf("opt in: %v", err)
	}

	w := dispatchRouting(t, p, "ws-log", "gpt-4o", simplePrompt, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := servedModel(t, sink); got != "gpt-4o-mini" {
		t.Fatalf("an opted-in workspace should route a concrete model to the cheap one, got %q", got)
	}
	if h := w.Header().Get("X-Talyvor-Routed"); h != "gpt-4o→gpt-4o-mini" {
		t.Fatalf("an opted-in routing substitution must be announced; header=%q", h)
	}
}

// An opted-in workspace still honours a genuinely complex prompt — no downgrade
// when the router would not pick a cheaper model (gpt-4o stays gpt-4o at premium).
func TestRoutingConsent_OptInComplexPromptNotDowngraded(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	if err := p.workspaceManager.SetCostOptimizeRouting(context.Background(), "ws-log", true); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	// Many complexity signals → premium tier (gpt-5.4, rank ABOVE gpt-4o) → no override.
	complex := "Write debug and explain a concurrent Go rate limiter step by step. func code import class why compare calculate derive proof"
	w := dispatchRouting(t, p, "ws-log", "gpt-4o", complex, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := servedModel(t, sink); got != "gpt-4o" {
		t.Fatalf("a complex prompt must not be downgraded (router picks premium, no cheaper override), got %q", got)
	}
}
