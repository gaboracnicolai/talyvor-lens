package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
)

// fakePinger is a stand-in for *pgxpool.Pool. It returns the configured
// error after sleeping for the configured delay so we can simulate
// "Postgres down" without needing pgxmock's ping support.
type fakePinger struct {
	err   error
	delay time.Duration
}

func (f *fakePinger) Ping(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func runEmbeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{
		Host:     "127.0.0.1",
		Port:     -1,
		StoreDir: t.TempDir(),
		NoLog:    true,
		NoSigs:   true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("natsserver.NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		srv.Shutdown()
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	return nc
}

// happyProviderServer returns 200 on HEAD so the provider check
// classifies as operational. Returned URL is shared by all four
// provider fields to keep the test fixture small.
func happyProviderServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestPage(t *testing.T, pinger pgxPinger) *StatusPage {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	nc := runEmbeddedNATS(t)
	srv := happyProviderServer(t)

	sp := newStatusPage(pinger, rc, nc, "0.1.0")
	sp.openaiURL = srv.URL
	sp.anthropicURL = srv.URL
	sp.googleURL = srv.URL
	sp.bedrockURL = srv.URL
	return sp
}

func TestCheck_ReturnsOperationalWhenAllHealthy(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	resp := sp.Check(context.Background())
	if resp.Status != StatusOperational {
		t.Errorf("overall = %q, want operational; components=%+v providers=%+v", resp.Status, resp.Components, resp.Providers)
	}
	for _, c := range resp.Components {
		if c.Status == StatusOutage {
			t.Errorf("component %s reported outage: %+v", c.Name, c)
		}
	}
}

func TestCheck_ReturnsOutageWhenPostgresDown(t *testing.T) {
	sp := newTestPage(t, &fakePinger{err: errors.New("connection refused")})
	resp := sp.Check(context.Background())
	if resp.Status != StatusOutage {
		t.Errorf("overall = %q, want outage", resp.Status)
	}
	var pg *Component
	for i := range resp.Components {
		if resp.Components[i].Name == "PostgreSQL" {
			pg = &resp.Components[i]
		}
	}
	if pg == nil || pg.Status != StatusOutage {
		t.Errorf("PostgreSQL component should be outage; got %+v", pg)
	}
}

func TestClassifyLatency_ThresholdsMatchSpec(t *testing.T) {
	cases := []struct {
		latency int64
		err     error
		want    ComponentStatus
	}{
		{50, nil, StatusOperational},
		{99, nil, StatusOperational},
		{100, nil, StatusOperational}, // == 100 is still healthy
		{150, nil, StatusDegraded},    // "slow Redis" lands here
		{499, nil, StatusDegraded},
		{501, nil, StatusOutage},
		{0, errors.New("boom"), StatusOutage},
	}
	for _, c := range cases {
		got := classifyLatency(c.latency, c.err)
		if got != c.want {
			t.Errorf("classifyLatency(%d, %v) = %q, want %q", c.latency, c.err, got, c.want)
		}
	}
}

func TestComputeOverall_IsWorstOfAllComponents(t *testing.T) {
	// All operational → operational.
	allOK := []ComponentStatus{StatusOperational, StatusOperational, StatusOperational}
	if got := computeOverall(allOK, nil); got != StatusOperational {
		t.Errorf("all-ok overall = %q, want operational", got)
	}
	// One degraded → degraded.
	oneDeg := []ComponentStatus{StatusOperational, StatusDegraded, StatusOperational}
	if got := computeOverall(oneDeg, nil); got != StatusDegraded {
		t.Errorf("one-degraded overall = %q, want degraded", got)
	}
	// One outage even with others healthy → outage.
	oneOut := []ComponentStatus{StatusOperational, StatusOutage, StatusOperational}
	if got := computeOverall(oneOut, nil); got != StatusOutage {
		t.Errorf("one-outage overall = %q, want outage", got)
	}
	// Provider outage still raises overall to outage.
	if got := computeOverall(allOK, []ComponentStatus{StatusOutage}); got != StatusOutage {
		t.Errorf("provider-outage overall = %q, want outage", got)
	}
}

func TestServeHTTP_ReturnsHTMLByDefault(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	sp.UpdateCache(sp.Check(context.Background()))

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	sp.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "TALYVOR LENS STATUS") {
		t.Errorf("HTML missing brand string")
	}
}

func TestServeHTTP_ReturnsJSONWhenAcceptIsApplicationJSON(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	sp.UpdateCache(sp.Check(context.Background()))

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	sp.ServeHTTP(w, req)

	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q, want application/json", w.Header().Get("Content-Type"))
	}
	var got StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Errorf("body is not valid JSON: %v", err)
	}
}

func TestServeJSON_AlwaysReturnsJSONRegardlessOfAccept(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	sp.UpdateCache(sp.Check(context.Background()))

	req := httptest.NewRequest(http.MethodGet, "/status.json", nil)
	req.Header.Set("Accept", "text/html") // try to confuse it
	w := httptest.NewRecorder()
	sp.ServeJSON(w, req)

	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type = %q, want application/json", w.Header().Get("Content-Type"))
	}
	var got StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Errorf("body is not valid JSON: %v", err)
	}
}

func TestUptimeHours_IncreasesOverTime(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	sp.startedAt = time.Now().Add(-90 * time.Minute) // booted 1.5h ago

	resp := sp.Check(context.Background())
	if resp.UptimeHours < 1.49 || resp.UptimeHours > 1.51 {
		t.Errorf("UptimeHours = %v, want ~1.50", resp.UptimeHours)
	}
	// Two-decimal-place precision asserted by the *.format* used in
	// the implementation — the test just checks the value isn't rounded
	// off to a whole number.
	if float64(int(resp.UptimeHours)) == resp.UptimeHours {
		t.Errorf("UptimeHours = %v, expected fractional precision", resp.UptimeHours)
	}
}

func TestStartCacher_UpdatesCachedResult(t *testing.T) {
	sp := newTestPage(t, &fakePinger{})
	// No cache yet.
	if sp.snapshot() != nil {
		t.Fatal("precondition: no cached snapshot")
	}

	// Drive one cycle synchronously.
	sp.runCheckOnce(context.Background())

	snap := sp.snapshot()
	if snap == nil {
		t.Fatal("snapshot is nil after runCheckOnce")
	}
	if snap.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt is zero — cache update didn't stamp the time")
	}
}
