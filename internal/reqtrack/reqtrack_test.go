package reqtrack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/reqtrack"
)

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// lines returns the decoded log lines whose msg is msg.
func (l *logBuf) lines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type healthBody struct {
	Requests reqtrack.Snapshot `json:"requests"`
}

func readHealthz(t *testing.T, base string) (healthBody, string) {
	t.Helper()
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var h healthBody
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("decode /healthz %q: %v", raw, err)
	}
	return h, string(raw)
}

// A request stuck on the provider shows on /healthz — in flight, the slowest of the window, by route
// template, in phase "upstream" — and in the log as it crosses the threshold and as it ends, while an
// SSE subscription open longer than it is counted but never named slowest. No raw path, so no id.
func TestSlowUpstreamRequestShowsOnHealthzAndInTheLog(t *testing.T) {
	const wsID = "ws_9f3a2b77"
	logs := &logBuf{}
	tr := reqtrack.New(50*time.Millisecond, slog.New(slog.NewJSONHandler(logs, nil)))

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	client := &http.Client{Transport: reqtrack.Transport(nil)}

	r := chi.NewRouter()
	r.Use(tr.Middleware(r))
	r.Get("/healthz", api.NewHealthHandler("test", map[string]api.HealthChecker{}).
		AddSection("requests", func(ctx context.Context) any { return tr.Snapshot(ctx) }).
		ForOperator(func(*http.Request) bool { return true }).ServeHTTP)
	r.Post("/v1/workspaces/{ws}/chat", func(w http.ResponseWriter, req *http.Request) {
		up, _ := http.NewRequestWithContext(req.Context(), http.MethodGet, upstream.URL, nil)
		resp, err := client.Do(up)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		_, _ = io.Copy(w, resp.Body)
		_ = resp.Body.Close()
	})
	sseDone := make(chan struct{})
	r.Get("/mcp/sse", func(w http.ResponseWriter, req *http.Request) {
		reqtrack.MarkLongLived(req.Context())
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-sseDone
	})
	lens := httptest.NewServer(r)
	defer lens.Close()

	sse, err := http.Get(lens.URL + "/mcp/sse")
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer func() { close(sseDone); _ = sse.Body.Close() }()

	finished := make(chan int, 1)
	go func() {
		resp, err := http.Post(lens.URL+"/v1/workspaces/"+wsID+"/chat", "application/json", nil)
		if err != nil {
			finished <- 0
			return
		}
		_ = resp.Body.Close()
		finished <- resp.StatusCode
	}()

	const route = "POST /v1/workspaces/{ws}/chat"
	var running healthBody
	waitFor(t, "the stuck request to cross the threshold on /healthz", func() bool {
		running, _ = readHealthz(t, lens.URL)
		s := running.Requests.Slowest5m
		return s != nil && s.Route == route && s.Ms >= 50 && len(logs.lines(t, "slow request still running")) == 1
	})
	if got := running.Requests; got.InFlight != 2 || got.LongLived != 1 || !got.Slowest5m.Running || got.Slowest5m.Phase != "upstream" {
		t.Fatalf("while stuck: want 2 in flight (1 long-lived), slowest running in upstream; got %+v / %+v", got, *got.Slowest5m)
	}
	crossing := logs.lines(t, "slow request still running")[0]
	if crossing["route"] != route || crossing["phase"] != "upstream" {
		t.Fatalf("crossing log: %v", crossing)
	}

	close(release)
	if code := <-finished; code != http.StatusOK {
		t.Fatalf("stuck request ended %d", code)
	}
	after, raw := readHealthz(t, lens.URL)
	s := after.Requests.Slowest5m
	if after.Requests.InFlight != 1 || s == nil || s.Running || s.Route != route || s.Phase != "upstream" || s.Ms < running.Requests.Slowest5m.Ms {
		t.Fatalf("after it ended: want 1 in flight (the SSE) and the finished request as slowest in upstream; got %+v / %+v", after.Requests, s)
	}
	ended := logs.lines(t, "slow request")
	if len(ended) != 1 || ended[0]["route"] != route || ended[0]["phase"] != "upstream" || ended[0]["ms"].(float64) < 50 {
		t.Fatalf("end-of-request log: %v", ended)
	}
	if strings.Contains(raw, wsID) || strings.Contains(logs.String(), wsID) {
		t.Fatalf("the workspace id leaked:\nhealthz %s\nlog %s", raw, logs.String())
	}
}

// A request that is waiting for a pool connection reads db_acquire, then db_query once it has one, and
// the pool section counts the connection in use and the wait (real PG: the pool is a real pgxpool).
func TestPoolWaitThenQueryShowAsTheirPhases_RealPG(t *testing.T) {
	dsn := os.Getenv("LENS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns, cfg.MinConns = 1, 0
	cfg.ConnConfig.Tracer = reqtrack.PgxTracer{}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	held, err := pool.Acquire(ctx) // the pool's only connection
	if err != nil {
		t.Fatal(err)
	}

	logs := &logBuf{}
	tr := reqtrack.New(50*time.Millisecond, slog.New(slog.NewJSONHandler(logs, nil)))
	r := chi.NewRouter()
	r.Use(tr.Middleware(r))
	r.Get("/v1/workspaces/{ws}/report", func(w http.ResponseWriter, req *http.Request) {
		if _, err := pool.Exec(req.Context(), "SELECT pg_sleep(0.4)"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	lens := httptest.NewServer(r)
	defer lens.Close()

	finished := make(chan int, 1)
	go func() {
		resp, err := http.Get(lens.URL + "/v1/workspaces/ws_1/report")
		if err != nil {
			finished <- 0
			return
		}
		_ = resp.Body.Close()
		finished <- resp.StatusCode
	}()

	phase := func(want string) func() bool {
		return func() bool {
			s := tr.Snapshot(ctx).Slowest5m
			return s != nil && s.Running && s.Phase == want
		}
	}
	waitFor(t, "db_acquire while the only connection is held, past the threshold", func() bool {
		return phase("db_acquire")() && len(logs.lines(t, "slow request still running")) == 1
	})
	if p := reqtrack.PoolStats(pool); p.InUse != 1 || p.Max != 1 {
		t.Fatalf("pool while held: %+v", p)
	}
	held.Release()
	waitFor(t, "db_query once the connection is free", phase("db_query"))
	if code := <-finished; code != http.StatusOK {
		t.Fatalf("request ended %d", code)
	}
	if p := reqtrack.PoolStats(pool); p.WaitCount < 1 {
		t.Fatalf("pool after: want the wait counted, got %+v", p)
	}
	ended := logs.lines(t, "slow request")
	if len(ended) != 1 || ended[0]["route"] != "GET /v1/workspaces/{ws}/report" || ended[0]["phase"] != "db_acquire" {
		t.Fatalf("a request slow because it waited for a connection is logged as db_acquire: %v", ended)
	}
}
