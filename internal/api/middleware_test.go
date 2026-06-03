package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ─── ParsePagination ─────────────────────────────

func newReqWithQuery(t *testing.T, q string) *http.Request {
	t.Helper()
	u, err := url.Parse("http://example.com/x?" + q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return &http.Request{URL: u, Header: http.Header{}}
}

func TestParsePagination_Defaults(t *testing.T) {
	r := newReqWithQuery(t, "")
	p := ParsePagination(r)
	if p.Page != 1 || p.PageSize != 20 || p.Cursor != "" {
		t.Fatalf("expected defaults, got %+v", p)
	}
}

func TestParsePagination_ClampsPageSize(t *testing.T) {
	r := newReqWithQuery(t, "page_size=500")
	p := ParsePagination(r)
	if p.PageSize != 100 {
		t.Fatalf("expected clamp to 100, got %d", p.PageSize)
	}
}

func TestParsePagination_HandlesInvalid(t *testing.T) {
	r := newReqWithQuery(t, "page=foo&page_size=bar")
	p := ParsePagination(r)
	if p.Page != 1 || p.PageSize != 20 {
		t.Fatalf("expected defaults on invalid input, got %+v", p)
	}
}

func TestParsePagination_NegativePageFalls(t *testing.T) {
	r := newReqWithQuery(t, "page=-5&page_size=-3")
	p := ParsePagination(r)
	if p.Page != 1 || p.PageSize != 20 {
		t.Fatalf("expected defaults for negatives, got %+v", p)
	}
}

func TestParsePagination_PassesCursor(t *testing.T) {
	r := newReqWithQuery(t, "cursor=abc123")
	p := ParsePagination(r)
	if p.Cursor != "abc123" {
		t.Fatalf("expected cursor=abc123, got %q", p.Cursor)
	}
}

// ─── NewPaginatedResponse ────────────────────────

func TestNewPaginatedResponse_TotalPages(t *testing.T) {
	r := NewPaginatedResponse([]int{1, 2, 3}, 1, 10, 25)
	if r.TotalPages != 3 {
		t.Fatalf("expected 3 pages (ceil(25/10)), got %d", r.TotalPages)
	}
}

func TestNewPaginatedResponse_HasNextHasPrev(t *testing.T) {
	r := NewPaginatedResponse([]int{}, 2, 10, 25)
	if !r.HasNext {
		t.Fatal("page 2 of 3 should have_next")
	}
	if !r.HasPrev {
		t.Fatal("page 2 should have_prev")
	}

	// last page
	r = NewPaginatedResponse([]int{}, 3, 10, 25)
	if r.HasNext {
		t.Fatal("last page should not have_next")
	}

	// first page
	r = NewPaginatedResponse([]int{}, 1, 10, 25)
	if r.HasPrev {
		t.Fatal("page 1 should not have_prev")
	}
}

func TestNewPaginatedResponse_ZeroTotal(t *testing.T) {
	r := NewPaginatedResponse([]int{}, 1, 10, 0)
	if r.TotalPages != 0 || r.HasNext || r.HasPrev {
		t.Fatalf("zero-total should have no pages and no nav, got %+v", r)
	}
}

// ─── WriteError / APIError ───────────────────────

func TestWriteError_ProducesCorrectJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, ErrCodeUnauthorized, "missing api key", http.StatusUnauthorized, "req-123")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	var got APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Code != "UNAUTHORIZED" || got.Message != "missing api key" || got.RequestID != "req-123" {
		t.Fatalf("unexpected envelope: %+v", got)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatal("missing JSON content type")
	}
}

func TestWriteErrorDetails_IncludesDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErrorDetails(rec, ErrCodeInvalidRequest, "bad", map[string]string{"field": "page_size"}, http.StatusBadRequest, "req-9")
	var got APIError
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Details == nil {
		t.Fatal("expected details to be present")
	}
}

// ─── RequestIDMiddleware ─────────────────────────

func TestRequestIDMiddleware_SetsHeader(t *testing.T) {
	m := &RequestIDMiddleware{}
	h := m.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestIDFromContext(r.Context()) == "" {
			t.Fatal("expected non-empty request id in context")
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("expected X-Request-ID response header to be set")
	}
}

func TestRequestIDMiddleware_UsesClientHeader(t *testing.T) {
	m := &RequestIDMiddleware{}
	var seen string
	h := m.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-ID", "trace-from-client")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "trace-from-client" {
		t.Fatalf("expected client request id, got %q", seen)
	}
	if rec.Header().Get("X-Request-ID") != "trace-from-client" {
		t.Fatalf("expected response header echo, got %q", rec.Header().Get("X-Request-ID"))
	}
}

// ─── APIVersionMiddleware ────────────────────────

func TestAPIVersionMiddleware_StampsHeader(t *testing.T) {
	h := APIVersionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Header().Get("X-API-Version") != APIVersion {
		t.Fatalf("expected version header %q, got %q", APIVersion, rec.Header().Get("X-API-Version"))
	}
}

// ─── GzipMiddleware ──────────────────────────────

func TestGzipMiddleware_SkipsSmallBodies(t *testing.T) {
	h := GzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("expected no gzip on small body")
	}
}

func TestGzipMiddleware_CompressesLargeBodies(t *testing.T) {
	big := strings.Repeat("a", 2048) // 2 KB, well over the 1 KB threshold
	h := GzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip Content-Encoding, got %q", rec.Header().Get("Content-Encoding"))
	}
	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	defer gz.Close()
	decoded, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(decoded) != big {
		t.Fatal("round-trip mismatch")
	}
}

func TestGzipMiddleware_SkipsWhenNotAccepted(t *testing.T) {
	big := strings.Repeat("a", 2048)
	h := GzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil) // no Accept-Encoding
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("must not gzip when client did not opt in")
	}
}

// ─── RateLimitHeadersMiddleware ──────────────────

func TestRateLimitHeaders_EmittedFromContext(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate upstream limiter stashing info on the ctx.
		ctx := WithRateLimit(r.Context(), RateLimitInfo{Limit: 1000, Remaining: 847, Reset: 1716912000})
		// We need the new context to take effect before WriteHeader fires.
		*r = *r.WithContext(ctx)
		w.WriteHeader(http.StatusOK)
	})
	h := RateLimitHeadersMiddleware(inner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Header().Get("X-RateLimit-Limit") != "1000" {
		t.Fatalf("expected limit=1000, got %q", rec.Header().Get("X-RateLimit-Limit"))
	}
	if rec.Header().Get("X-RateLimit-Remaining") != "847" {
		t.Fatalf("expected remaining=847, got %q", rec.Header().Get("X-RateLimit-Remaining"))
	}
	if rec.Header().Get("X-RateLimit-Reset") != "1716912000" {
		t.Fatalf("expected reset=1716912000, got %q", rec.Header().Get("X-RateLimit-Reset"))
	}
}

// ─── HealthHandler ───────────────────────────────

func TestHealthHandler_HealthyWhenAllPass(t *testing.T) {
	h := NewHealthHandler("0.1.0", map[string]HealthChecker{
		"db":    HealthCheckFunc(func(_ context.Context) (bool, int64, string) { return true, 2, "" }),
		"redis": HealthCheckFunc(func(_ context.Context) (bool, int64, string) { return true, 1, "" }),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "healthy" {
		t.Fatalf("expected healthy, got %v", got["status"])
	}
}

func TestHealthHandler_DegradedWhenDetailPresent(t *testing.T) {
	h := NewHealthHandler("0.1.0", map[string]HealthChecker{
		"local_models": HealthCheckFunc(func(_ context.Context) (bool, int64, string) {
			return true, 5, "1/3 endpoints unhealthy"
		}),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "degraded" {
		t.Fatalf("expected degraded, got %v", got["status"])
	}
}

func TestHealthHandler_Unhealthy503(t *testing.T) {
	h := NewHealthHandler("0.1.0", map[string]HealthChecker{
		"db": HealthCheckFunc(func(_ context.Context) (bool, int64, string) { return false, 0, "connection refused" }),
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestHealthHandler_RespondsUnder100ms(t *testing.T) {
	h := NewHealthHandler("0.1.0", map[string]HealthChecker{
		"slow": HealthCheckFunc(func(ctx context.Context) (bool, int64, string) {
			select {
			case <-time.After(500 * time.Millisecond):
				return true, 500, ""
			case <-ctx.Done():
				return false, 0, "timeout"
			}
		}),
	})
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("health check took %v, expected <200ms", elapsed)
	}
}

// ─── OpenAPI ─────────────────────────────────────

func TestOpenAPISpec_IsValidJSON(t *testing.T) {
	buf, err := json.Marshal(OpenAPISpec())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundtrip map[string]any
	if err := json.Unmarshal(buf, &roundtrip); err != nil {
		t.Fatalf("roundtrip unmarshal: %v", err)
	}
	if roundtrip["openapi"] != "3.0.3" {
		t.Fatalf("expected openapi=3.0.3, got %v", roundtrip["openapi"])
	}
	if _, ok := roundtrip["paths"].(map[string]any); !ok {
		t.Fatal("expected paths object")
	}
}

func TestServeOpenAPI_RespondsJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeOpenAPI(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatal("expected application/json content type")
	}
}

// ─── SecurityHeadersMiddleware ───────────────────

func TestSecurityHeadersMiddleware_SetsAllHeaders(t *testing.T) {
	h := SecurityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy = %q, want strict-origin-when-cross-origin", got)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("Content-Security-Policy must be set")
	}
	for _, directive := range []string{
		"default-src 'self'",
		"frame-ancestors 'none'",
		"https://fonts.googleapis.com",
		"https://fonts.gstatic.com",
		"base-uri 'self'",
		"form-action 'self'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing directive %q; full value: %s", directive, csp)
		}
	}
}

func TestSecurityHeadersMiddleware_DoesNotBlockDownstream(t *testing.T) {
	called := false
	h := SecurityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if !called {
		t.Fatal("downstream handler was not called")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.Code)
	}
}

// ─── CORSMiddleware ──────────────────────────────

func TestCORSMiddleware_EmptyOrigins_IsNoOp(t *testing.T) {
	called := false
	h := CORSMiddleware("")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatal("downstream must still be called")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("no ACAO header should be set when CORS is disabled")
	}
}

func TestCORSMiddleware_AllowedOrigin_SetsHeaders(t *testing.T) {
	h := CORSMiddleware("https://app.example.com")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("ACAO = %q, want https://app.example.com", got)
	}
	if rec.Header().Get("Vary") == "" {
		t.Error("Vary header must be set when ACAO is present")
	}
}

func TestCORSMiddleware_DisallowedOrigin_NoHeaders(t *testing.T) {
	h := CORSMiddleware("https://app.example.com")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://other.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("ACAO must not be set for a disallowed origin")
	}
}

func TestCORSMiddleware_Wildcard_AllowsAnyOrigin(t *testing.T) {
	h := CORSMiddleware("*")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://random.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://random.example.com" {
		t.Errorf("wildcard CORS: ACAO = %q, want the actual origin", got)
	}
}

func TestCORSMiddleware_Preflight_Returns204(t *testing.T) {
	h := CORSMiddleware("https://app.example.com")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Should not be reached on preflight.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", rec.Code)
	}
}

func TestCORSMiddleware_MultipleOrigins_AllAllowed(t *testing.T) {
	h := CORSMiddleware("https://a.example.com, https://b.example.com")(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)
	for _, origin := range []string{"https://a.example.com", "https://b.example.com"} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("origin %q: ACAO = %q, want match", origin, got)
		}
	}
}
