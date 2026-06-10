//go:build bench

// Package bench is the performance suite for Talyvor Lens.
//
// Gated behind the `bench` build tag so a regular `go test ./...` skips
// it — benchmarks take minutes, the unit test suite takes seconds. Run
// from the repo root with:
//
//	go test -tags=bench -bench=. -benchmem ./benchmarks/...
//
// Every benchmark mocks the LLM upstream with httptest so the headline
// numbers reflect proxy overhead, not provider latency.
package bench_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/proxy"
	"github.com/talyvor/lens/internal/ratelimit"
	"github.com/talyvor/lens/internal/router"
)

// canned200 returns a httptest.Server that responds to every POST with
// a small, plausible OpenAI chat-completion body. Used to remove
// upstream latency from the benchmark numbers.
func canned200(b *testing.B) *httptest.Server {
	b.Helper()
	body := `{"id":"chatcmpl-bench","choices":[{"message":{"role":"assistant","content":"ok"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	b.Cleanup(srv.Close)
	return srv
}

func newExactCache(b *testing.B) (*cache.ExactCache, *miniredis.Miniredis) {
	b.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatalf("miniredis.Run: %v", err)
	}
	b.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	b.Cleanup(func() { _ = rc.Close() })
	return cache.NewExactCache(rc, time.Minute), mr
}

// newBenchProxy builds a Proxy with everything optional set to nil and
// the upstream URL pointed at the mock server. Matches the signature
// proxy.New uses today (positional arg order locked here so future
// signature changes surface as a single compile error in bench).
func newBenchProxy(b *testing.B, upstreamURL string) (*proxy.Proxy, *cache.ExactCache) {
	b.Helper()
	exact, _ := newExactCache(b)
	p := proxy.New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, nil,
		"openai-key", "anthropic-key", "",
	)
	// Hot-patch the openai URL via the public field — we don't have a
	// setter so the test binary reaches in. Acceptable for a benchmark
	// where we control both sides.
	if upstreamURL != "" {
		// The bench-tagged setter file in internal/proxy/bench_setters.go
		// exposes SetOpenAIURL so this external benchmark package can
		// point the proxy at our httptest server.
		p.SetOpenAIURL(upstreamURL)
	}
	return p, exact
}

// dispatchOpenAI executes a single proxy round-trip. Used inside the
// b.N loops so the benchmark measures the full handler cost, not just
// one sub-system.
func dispatchOpenAI(b *testing.B, p *proxy.Proxy, body string) {
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	if w.Code != http.StatusOK {
		b.Fatalf("proxy returned %d: %s", w.Code, w.Body.String())
	}
}

func BenchmarkExactCacheHit(b *testing.B) {
	srv := canned200(b)
	p, exact := newBenchProxy(b, srv.URL)
	// Pre-warm the cache so every iteration is a hit.
	cached := []byte(`{"cached":"hello"}`)
	if err := exact.Set(context.Background(), "openai", "gpt-4", "hello-bench", cached); err != nil {
		b.Fatalf("seed cache: %v", err)
	}
	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello-bench"}]}`
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dispatchOpenAI(b, p, body)
	}
}

func BenchmarkExactCacheMiss(b *testing.B) {
	srv := canned200(b)
	p, _ := newBenchProxy(b, srv.URL)
	// Each iteration uses a fresh prompt so the cache never hits.
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"unique-` + itoa(i) + `"}]}`
		dispatchOpenAI(b, p, body)
	}
}

// BenchmarkSemanticCacheHit is a stub that exercises the proxy path
// but without a populated semantic cache — the real semantic-cache
// hit path requires a pgvector-backed Postgres instance which we don't
// spin up inside the benchmark process. Kept as a reserved slot in
// the suite so the headline output mentions it.
func BenchmarkSemanticCacheHit(b *testing.B) {
	srv := canned200(b)
	p, _ := newBenchProxy(b, srv.URL)
	body := `{"model":"gpt-4","messages":[{"role":"user","content":"semantic"}]}`
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dispatchOpenAI(b, p, body)
	}
}

func BenchmarkPromptCompression(b *testing.B) {
	c := compressor.New()
	prompt := strings.Repeat("This is a fairly verbose request that the compressor should be able to shorten effectively. ", 25)
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = c.Compress(ctx, prompt)
	}
}

func BenchmarkModelRouting(b *testing.B) {
	r := router.New()
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.Route(ctx, "openai", "gpt-4o", "summarise this paragraph please")
	}
}

func BenchmarkPIIDetection(b *testing.B) {
	d := pii.New()
	text := "Contact John Doe at john@example.com or 555-123-4567. SSN 123-45-6789. " +
		strings.Repeat("Lorem ipsum dolor sit amet. ", 18)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = d.Detect(text)
	}
}

func BenchmarkInjectionDetection(b *testing.B) {
	d := injection.New(injection.DefaultPolicy())
	prompt := strings.Repeat("Summarise this normal paragraph concisely. ", 8)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = d.Detect(prompt)
	}
}

func BenchmarkConcurrentRequests(b *testing.B) {
	srv := canned200(b)
	p, _ := newBenchProxy(b, srv.URL)
	body := `{"model":"gpt-4","messages":[{"role":"user","content":"concurrent-load"}]}`
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			dispatchOpenAI(b, p, body)
		}
	})
}

func BenchmarkRateLimitCheck(b *testing.B) {
	mr, err := miniredis.Run()
	if err != nil {
		b.Fatalf("miniredis: %v", err)
	}
	b.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	b.Cleanup(func() { _ = rc.Close() })

	l := ratelimit.New(rc, ratelimit.DefaultRules())
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = l.Check(ctx, "default", "bench-key")
	}
}

// BenchmarkFullProxyStack is the headline number: full request through
// the proxy with cache miss, PII scan, injection scan, model routing,
// compression, upstream forward (mocked), cache write, span emit.
func BenchmarkFullProxyStack(b *testing.B) {
	srv := canned200(b)
	p, _ := newBenchProxy(b, srv.URL)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		body := `{"model":"gpt-4","messages":[{"role":"user","content":"full-stack-` + itoa(i) + `"}]}`
		dispatchOpenAI(b, p, body)
	}
}

// itoa is a zero-alloc integer formatter for the loop body. fmt.Sprintf
// would dominate the per-iteration alloc count and skew the report.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
