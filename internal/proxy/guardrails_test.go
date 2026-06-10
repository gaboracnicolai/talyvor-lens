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

// newGuardrailProxy builds a proxy whose upstream returns a fixed assistant
// content, and exposes the guardrails engine so tests can configure the
// output stage.
func newGuardrailProxy(t *testing.T, content string) (*Proxy, *guardrails.Engine, *recordingAlertSink) {
	t.Helper()
	body := `{"choices":[{"message":{"role":"assistant","content":` + jsonQuote(content) + `}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "ws-g", Name: "guard", Active: true, LoggingPolicy: workspace.LoggingMetadata,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	eng := guardrails.New(pii.New(), injection.New(injection.DefaultPolicy()))
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, eng,
		"openai-key", "anthropic-key", "",
	)
	p.openAIURL = srv.URL
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)
	return p, eng, sink
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func dispatchG(t *testing.T, p *Proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-g")
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	return w
}

const textReq = `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`

// Output guardrails on a NON-streaming response: a forbidden-pattern block
// fails fast with 422, doesn't return the content, and records NO spend.
func TestOutputGuardrail_BlockNonStreaming(t *testing.T) {
	p, eng, sink := newGuardrailProxy(t, "the secret is hunter2")
	eng.SetOutputEnabled(true)
	eng.SetPolicy(context.Background(), "ws-g", guardrails.GuardrailPolicy{
		OutputMustNotMatch: "(?i)secret", OutputValidationBlock: true,
	})

	w := dispatchG(t, p, textReq)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Talyvor-Output-Guardrail-Blocked") != "true" {
		t.Fatal("blocked output must set the blocked header")
	}
	if strings.Contains(w.Body.String(), "hunter2") {
		t.Fatal("blocked content must NOT be returned to the client")
	}
	if sink.calls != 0 {
		t.Fatalf("a blocked output must record no spend: calls=%d", sink.calls)
	}
}

// Output redaction masks PII in the response and still returns 200.
func TestOutputGuardrail_RedactNonStreaming(t *testing.T) {
	p, eng, sink := newGuardrailProxy(t, "email me at jane.roe@example.com")
	eng.SetOutputEnabled(true)
	eng.SetPolicy(context.Background(), "ws-g", guardrails.GuardrailPolicy{OutputPIIAction: guardrails.ActionRedact})

	w := dispatchG(t, p, textReq)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Talyvor-Output-Redacted") != "true" {
		t.Fatal("redacted output must set the redacted header")
	}
	if strings.Contains(w.Body.String(), "jane.roe@example.com") {
		t.Fatalf("PII must be masked in the returned response: %s", w.Body.String())
	}
	if sink.calls != 1 {
		t.Fatalf("redacted (allowed) response should record spend once: calls=%d", sink.calls)
	}
}

// Disabled output stage → behaves as today: the would-block response passes
// through untouched and is billed normally.
func TestOutputGuardrail_DisabledBehavesAsToday(t *testing.T) {
	p, eng, sink := newGuardrailProxy(t, "the secret is hunter2")
	// Output stage left OFF; policy would block if it were on.
	eng.SetPolicy(context.Background(), "ws-g", guardrails.GuardrailPolicy{
		OutputMustNotMatch: "(?i)secret", OutputValidationBlock: true,
	})

	w := dispatchG(t, p, textReq)
	if w.Code != http.StatusOK {
		t.Fatalf("disabled output stage must pass through: status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hunter2") {
		t.Fatal("disabled output stage must return the original content")
	}
	if sink.calls != 1 {
		t.Fatalf("normal response should record spend once: calls=%d", sink.calls)
	}
}
