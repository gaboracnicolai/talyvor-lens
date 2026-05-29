package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// scrape renders the /metrics output via the real promhttp handler.
func scrape(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", w.Code)
	}
	body, _ := io.ReadAll(w.Result().Body)
	return string(body)
}

// TestAllMetricNamesExposed touches every metric once, then asserts each name
// appears on /metrics. (Vec metrics only appear once a label set is observed.)
func TestAllMetricNamesExposed(t *testing.T) {
	RecordUpstream("openai", "200", 12*time.Millisecond)
	RecordCacheHit("exact")
	RecordCacheMiss("semantic")
	SetBreakerState("anthropic", 1)
	RateLimitRejected("ws-1")
	ObserveLedgerWrite(3 * time.Millisecond)
	MintedTokens(5)
	ConvertedLXC(2)
	SetHAInstanceCount(3)
	InflightRequests.Set(0)
	HTTPRequestsTotal.WithLabelValues("/v1/x", "GET", "200").Inc()
	HTTPRequestDuration.WithLabelValues("/v1/x", "GET").Observe(0.01)

	body := scrape(t)
	for _, name := range []string{
		"lens_http_requests_total",
		"lens_http_request_duration_seconds",
		"lens_inflight_requests",
		"lens_upstream_provider_requests_total",
		"lens_upstream_provider_duration_seconds",
		"lens_cache_hits_total",
		"lens_cache_misses_total",
		"lens_circuit_breaker_state",
		"lens_rate_limit_rejections_total",
		"lens_token_ledger_write_duration_seconds",
		"lens_tokens_minted_total",
		"lens_lxc_converted_total",
		"lens_ha_instance_count",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics is missing %q", name)
		}
	}
}

// TestHTTPMiddlewareCountsRequests verifies the middleware increments
// http_requests_total with the route PATTERN (not the raw path) and the status.
func TestHTTPMiddlewareCountsRequests(t *testing.T) {
	r := chi.NewRouter()
	r.Use(HTTPMiddleware)
	r.Get("/v1/items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/items/abc123")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	body := scrape(t)
	// Route is the pattern with {id}, NOT the raw "abc123" — proves bounded
	// cardinality. Status 418 from the handler.
	want := `lens_http_requests_total{method="GET",route="/v1/items/{id}",status="418"}`
	if !strings.Contains(body, want) {
		t.Errorf("expected counter line %q in:\n%s", want, body)
	}
	if strings.Contains(body, "abc123") {
		t.Error("raw path id leaked into a metric label — cardinality risk")
	}
}
