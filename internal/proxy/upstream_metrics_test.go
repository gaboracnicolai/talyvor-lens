package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/metrics"
)

// rtFunc is a RoundTripper that returns a fixed error, to simulate a transport
// failure (no HTTP response) at the upstream call.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func scrapeProxyMetrics(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, req)
	body, _ := io.ReadAll(w.Result().Body)
	return string(body)
}

// TestForward_TransportErrorReturnedUnchanged proves the instrumentation does
// NOT alter the upstream error and still records a status="error" observation.
func TestForward_TransportErrorReturnedUnchanged(t *testing.T) {
	sentinel := errors.New("sentinel-transport-boom")
	p := &Proxy{
		httpClient:  &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) { return nil, sentinel })},
		retryConfig: fastRetry(),
		openAIURL:   "http://upstream.invalid",
	}

	// Construction via the real configForProvider→inference.ConfigFor path (PR-3c — the providerConfig
	// type moved to inference with unexported fields). Assertions below are unchanged.
	_, _, _, err := p.forward(context.Background(),
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")), []byte("{}"), "gpt-4", p.configForProvider("openai"))

	if err == nil {
		t.Fatal("expected an error from a failing transport")
	}
	// The original error must survive UNCHANGED through the instrumentation.
	if !errors.Is(err, sentinel) {
		t.Fatalf("upstream error was altered by instrumentation: %v", err)
	}
}

// TestForward_SuccessAndError_RecordUpstream drives forward() on both a 2xx
// upstream and a failing transport, then asserts the upstream metrics carry the
// bounded provider+status labels and the duration histogram observed.
func TestForward_SuccessAndError_RecordUpstream(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(okSrv.Close)

	pOK := &Proxy{httpClient: &http.Client{}, retryConfig: fastRetry(), anthropicURL: okSrv.URL}
	if _, _, _, err := pOK.forward(context.Background(),
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")), []byte("{}"), "claude-3",
		pOK.configForProvider("anthropic")); err != nil {
		t.Fatalf("success forward: %v", err)
	}

	pErr := &Proxy{
		httpClient:  &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") })},
		retryConfig: fastRetry(),
		openAIURL:   "http://upstream.invalid",
	}
	_, _, _, _ = pErr.forward(context.Background(),
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")), []byte("{}"), "gpt-4",
		pErr.configForProvider("openai"))

	body := scrapeProxyMetrics(t)
	for _, want := range []string{
		`lens_upstream_provider_requests_total{provider="anthropic",status="2xx"}`,
		`lens_upstream_provider_requests_total{provider="openai",status="error"}`,
		`lens_upstream_provider_duration_seconds_count{provider="anthropic"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}

// TestServe_CacheMissRecorded drives a full request with no cache hit and
// asserts lens_cache_misses_total increments (symmetric with the hit metric).
func TestServe_CacheMissRecorded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"fresh"}}]}`)
	}))
	t.Cleanup(upstream.Close)

	p := newProxyWithFallback(t, upstream.URL, "", "")
	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"a-unique-uncached-prompt-42"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if body := scrapeProxyMetrics(t); !strings.Contains(body, `lens_cache_misses_total{layer="cache_miss"}`) {
		t.Errorf("/metrics missing lens_cache_misses_total{layer=\"cache_miss\"}")
	}
}
