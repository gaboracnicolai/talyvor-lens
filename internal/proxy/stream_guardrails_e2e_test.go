package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
)

// piiEmail is content an output guardrail (output PII) must catch.
const piiEmail = "sure — reach me at john.doe@example.com anytime"

// bufferAwareUpstream mimics a real provider: it SSE-streams when the forwarded
// request has "stream":true, and returns a normal JSON completion otherwise.
// Both carry `content`. This lets the test prove whether the BUFFERED path
// actually produces an inspectable (non-streamed) response — if buffering
// failed to strip streaming, the upstream would SSE and the output guardrail
// couldn't see the content.
func bufferAwareUpstream(t *testing.T, content string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var rb struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(raw, &rb)
		if rb.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":"+strconv.Quote(content)+"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": content}},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// e2eProxy builds a proxy with the given guardrails engine wired in (mirrors the
// constructor used by the other stream tests).
func e2eProxy(t *testing.T, eng *guardrails.Engine, upstreamURL string) *Proxy {
	t.Helper()
	exact, _ := newExactCacheForTest(t)
	p := New(exact, nil, nil, compressor.New(), router.New(), pii.New(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, fallback.New(), nil, nil, eng, "openai-key", "anthropic-key", "")
	p.openAIURL = upstreamURL
	return p
}

func streamReq(t *testing.T) *http.Request {
	t.Helper()
	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// THE E2E (now FIXED): a streamed request with output guardrails +
// BufferStreamForOutput opted in, whose buffered output contains PII, has the
// output guardrail FIRE on the buffered content → BLOCK (422), not a leak. This
// proves streaming + buffering + output-guardrail evaluation work together: the
// upstream call is forced non-streaming (so CheckOutput can inspect a parseable
// completion), and the PII never reaches the client.
func TestStreamGuardrails_BufferedOutputPIIBlocks(t *testing.T) {
	eng := guardrails.New(pii.New(), injection.New(injection.DefaultPolicy()))
	eng.SetOutputEnabled(true)
	_ = eng.SetPolicy(context.Background(), "default", guardrails.GuardrailPolicy{
		BufferStreamForOutput: true,
		OutputPIIAction:       guardrails.ActionBlock,
	})
	p := e2eProxy(t, eng, bufferAwareUpstream(t, piiEmail).URL)

	w := newFlushRecorder()
	p.HandleOpenAI(w, streamReq(t))

	if got := w.Header().Get("X-Talyvor-Stream-Buffered"); got != "true" {
		t.Errorf("expected the stream to be buffered (X-Talyvor-Stream-Buffered=true), got %q", got)
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("buffered PII output must be BLOCKED (422), got %d — body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "output guardrail violation") {
		t.Errorf("block response should name the output guardrail; got %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "john.doe@example.com") {
		t.Errorf("blocked response leaked the PII content")
	}
}

// REDACT action on a buffered stream: PII is masked in the delivered response
// (not blocked), the client still gets a stream-shaped (SSE) response with the
// PII removed, and X-Talyvor-Output-Redacted is set. (The masked result is what
// gets billed/cached — see the spend gating in proxy.go.)
func TestStreamGuardrails_BufferedOutputPIIRedacts(t *testing.T) {
	eng := guardrails.New(pii.New(), injection.New(injection.DefaultPolicy()))
	eng.SetOutputEnabled(true)
	_ = eng.SetPolicy(context.Background(), "default", guardrails.GuardrailPolicy{
		BufferStreamForOutput: true,
		OutputPIIAction:       guardrails.ActionRedact,
	})
	p := e2eProxy(t, eng, bufferAwareUpstream(t, piiEmail).URL)

	w := newFlushRecorder()
	p.HandleOpenAI(w, streamReq(t))

	if got := w.Header().Get("X-Talyvor-Stream-Buffered"); got != "true" {
		t.Errorf("expected buffered, got %q", got)
	}
	if got := w.Header().Get("X-Talyvor-Output-Redacted"); got != "true" {
		t.Errorf("expected X-Talyvor-Output-Redacted=true, got %q", got)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("redacted output must pass (200), got %d — %s", w.Code, w.Body.String())
	}
	// Client gets a stream-shaped (SSE) response…
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("buffered success should be delivered as SSE, got Content-Type %q", got)
	}
	if !strings.HasPrefix(w.Body.String(), "data: ") || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Errorf("buffered success should be a single SSE event; got %s", w.Body.String())
	}
	// …with the PII masked out.
	if strings.Contains(w.Body.String(), "john.doe@example.com") {
		t.Errorf("redacted response still contains the PII: %s", w.Body.String())
	}
}

// A CLEAN buffered stream passes through and reaches the client intact, as a
// stream-shaped (SSE) response.
func TestStreamGuardrails_BufferedCleanReachesClient(t *testing.T) {
	eng := guardrails.New(pii.New(), injection.New(injection.DefaultPolicy()))
	eng.SetOutputEnabled(true)
	_ = eng.SetPolicy(context.Background(), "default", guardrails.GuardrailPolicy{
		BufferStreamForOutput: true,
		OutputPIIAction:       guardrails.ActionBlock,
	})
	clean := "here is a perfectly clean answer with no sensitive data"
	p := e2eProxy(t, eng, bufferAwareUpstream(t, clean).URL)

	w := newFlushRecorder()
	p.HandleOpenAI(w, streamReq(t))

	if w.Code != http.StatusOK {
		t.Fatalf("clean buffered output must pass (200), got %d — %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("clean buffered success should be SSE-shaped, got %q", got)
	}
	if !strings.Contains(w.Body.String(), "clean answer") || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Errorf("clean content should reach the client as an SSE event; got %s", w.Body.String())
	}
}

// THE OPT-IN BOUNDARY: output guardrails enabled but BufferStreamForOutput is
// NOT opted in → the response streams (SSE), is marked not-applied-streaming,
// and output guardrails do NOT run (PII passes through unredacted). This proves
// the honest default: streaming bypasses output guardrails unless buffering is
// explicitly chosen.
func TestStreamGuardrails_NotBufferedSkipsOutputGuardrails(t *testing.T) {
	eng := guardrails.New(pii.New(), injection.New(injection.DefaultPolicy()))
	eng.SetOutputEnabled(true)
	_ = eng.SetPolicy(context.Background(), "default", guardrails.GuardrailPolicy{
		BufferStreamForOutput: false, // NOT opted in
		OutputPIIAction:       guardrails.ActionBlock,
	})
	p := e2eProxy(t, eng, bufferAwareUpstream(t, piiEmail).URL)

	w := newFlushRecorder()
	p.HandleOpenAI(w, streamReq(t))

	if got := w.Header().Get("X-Talyvor-Stream-Buffered"); got == "true" {
		t.Errorf("must NOT buffer when not opted in")
	}
	if got := w.Header().Get("X-Talyvor-Output-Guardrails"); got != "not-applied-streaming" {
		t.Errorf("streamed response must be marked not-applied-streaming, got %q", got)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("streamed response status = %d, want 200", w.Code)
	}
	// Output guardrails did NOT run → the PII streamed through unredacted.
	if !strings.Contains(w.Body.String(), "john.doe@example.com") {
		t.Errorf("non-buffered stream should pass output through untouched (guardrails skipped); body=%s", w.Body.String())
	}
}
