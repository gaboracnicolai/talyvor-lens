package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
)

// setupOTel installs a tracer provider backed by an in-memory exporter
// and the W3C TraceContext propagator. Returns the exporter so tests
// can introspect captured spans. Restores the previous globals on cleanup
// so tests don't bleed into each other.
func setupOTel(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))

	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return exp
}

func newProxyWithMockUpstream(t *testing.T, srvURL string, primeCache func(c *cacheForTest)) (*Proxy, *cacheForTest) {
	t.Helper()
	exact, mr := newExactCacheForTest(t)
	c := &cacheForTest{exact: exact, mr: mr}
	if primeCache != nil {
		primeCache(c)
	}
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	if srvURL != "" {
		p.openAIURL = srvURL
	}
	return p, c
}

// cacheForTest is a thin wrapper to give the test helper room to seed
// the cache before constructing the proxy.
type cacheForTest struct {
	exact interface{ /* seeded outside */ }
	mr    interface{}
}

func TestOTel_XTalyvorTraceIDHeaderSetOnResponse(t *testing.T) {
	setupOTel(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(srv.Close)

	p, _ := newProxyWithMockUpstream(t, srv.URL, nil)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	got := w.Header().Get("X-Talyvor-Trace-ID")
	if got == "" {
		t.Fatal("X-Talyvor-Trace-ID header missing on response")
	}
	if len(got) != 32 {
		t.Errorf("X-Talyvor-Trace-ID = %q, want 32 hex chars", got)
	}
}

func TestOTel_TraceparentPropagatesToResponseTraceID(t *testing.T) {
	setupOTel(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(srv.Close)

	p, _ := newProxyWithMockUpstream(t, srv.URL, nil)

	const wantTraceID = "0af7651916cd43dd8448eb211c80319c"
	traceparent := fmt.Sprintf("00-%s-b7ad6b7169203331-01", wantTraceID)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", traceparent)
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)

	if got := w.Header().Get("X-Talyvor-Trace-ID"); got != wantTraceID {
		t.Errorf("X-Talyvor-Trace-ID = %q, want %q (traceparent propagation broken)", got, wantTraceID)
	}
}

func TestOTel_SpanCreatedForEachRequest(t *testing.T) {
	exp := setupOTel(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(srv.Close)

	p, _ := newProxyWithMockUpstream(t, srv.URL, nil)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	p.HandleOpenAI(httptest.NewRecorder(), req)

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no spans captured")
	}
	var serveSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "proxy.serve" {
			serveSpan = &spans[i]
			break
		}
	}
	if serveSpan == nil {
		t.Fatalf("proxy.serve span not found; got spans: %+v", spanNames(spans))
	}
}

func TestOTel_CacheHitAddsSpanEvent(t *testing.T) {
	exp := setupOTel(t)

	// No upstream — the cache hit should short-circuit before any LLM call.
	p, _ := newProxyWithMockUpstream(t, "", nil)
	if err := p.exact.Set(context.Background(), "openai", "gpt-4", "hi", []byte(`{"choices":[]}`)); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	p.HandleOpenAI(httptest.NewRecorder(), req)

	var serve *tracetest.SpanStub
	for i, s := range exp.GetSpans() {
		if s.Name == "proxy.serve" {
			serve = &exp.GetSpans()[i]
			break
		}
	}
	if serve == nil {
		t.Fatal("proxy.serve span not captured")
	}

	hitFound := false
	for _, ev := range serve.Events {
		if ev.Name == "cache.hit.exact" {
			hitFound = true
		}
	}
	if !hitFound {
		var evs []string
		for _, ev := range serve.Events {
			evs = append(evs, ev.Name)
		}
		t.Errorf("cache.hit.exact event missing; got events %v", evs)
	}
}

func TestOTel_UpstreamRequestGetsTraceparentInjected(t *testing.T) {
	setupOTel(t)

	gotTraceparent := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent <- r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(srv.Close)

	p, _ := newProxyWithMockUpstream(t, srv.URL, nil)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	p.HandleOpenAI(httptest.NewRecorder(), req)

	select {
	case tp := <-gotTraceparent:
		if tp == "" {
			t.Fatal("upstream request did not receive a traceparent header")
		}
		if !strings.HasPrefix(tp, "00-") {
			t.Errorf("traceparent doesn't look like W3C format: %q", tp)
		}
	default:
		t.Fatal("upstream was not hit")
	}
}

// helpers ------------------------------------------------------------

func spanNames(spans []tracetest.SpanStub) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}
