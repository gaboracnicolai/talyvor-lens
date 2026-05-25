package localrouter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ─── ParseEndpointsConfig ────────────────────────

func TestParseEndpointsConfig_Good(t *testing.T) {
	cfg := "ollama:http://localhost:11434:llama3.1,mistral;vllm:http://localhost:8000:codellama"
	out, err := ParseEndpointsConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(out))
	}
	if out[0].Provider != "ollama" || out[0].URL != "http://localhost:11434" {
		t.Fatalf("bad parse[0]: %+v", out[0])
	}
	if len(out[0].Models) != 2 || out[0].Models[0] != "llama3.1" || out[0].Models[1] != "mistral" {
		t.Fatalf("bad models[0]: %+v", out[0].Models)
	}
	if out[1].Provider != "vllm" || out[1].Models[0] != "codellama" {
		t.Fatalf("bad parse[1]: %+v", out[1])
	}
}

func TestParseEndpointsConfig_Empty(t *testing.T) {
	out, err := ParseEndpointsConfig("")
	if err != nil || len(out) != 0 {
		t.Fatalf("expected (nil, nil), got (%v, %v)", out, err)
	}
}

func TestParseEndpointsConfig_Malformed(t *testing.T) {
	// One good, two malformed.
	cfg := "ollama:http://localhost:11434:llama3.1;bogus:nourl;vllm::nomodels"
	out, err := ParseEndpointsConfig(cfg)
	if err == nil {
		t.Fatal("expected an error reporting skipped entries")
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 good endpoint, got %d", len(out))
	}
	if out[0].Provider != "ollama" {
		t.Fatalf("unexpected good provider: %q", out[0].Provider)
	}
}

func TestParseEndpointsConfig_UnknownProviderSkipped(t *testing.T) {
	out, err := ParseEndpointsConfig("triton:http://localhost:9000:any")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if len(out) != 0 {
		t.Fatalf("expected 0 endpoints, got %d", len(out))
	}
}

// ─── SelectEndpoint ──────────────────────────────

func TestSelectEndpoint_RoundRobinCycles(t *testing.T) {
	r := NewRouter(nil)
	a := &LocalEndpoint{ID: "a", URL: "http://a", Provider: "ollama", Models: []string{"m1"}, Healthy: true}
	b := &LocalEndpoint{ID: "b", URL: "http://b", Provider: "ollama", Models: []string{"m1"}, Healthy: true}
	r.Register(a)
	r.Register(b)
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		ep, err := r.SelectEndpoint("m1", StrategyRoundRobin)
		if err != nil {
			t.Fatalf("SelectEndpoint: %v", err)
		}
		seen[ep.ID]++
	}
	if seen["a"] != 3 || seen["b"] != 3 {
		t.Fatalf("expected 3+3, got %+v", seen)
	}
}

func TestSelectEndpoint_SkipsUnhealthy(t *testing.T) {
	r := NewRouter(nil)
	r.Register(&LocalEndpoint{ID: "a", URL: "http://a", Provider: "ollama", Models: []string{"m1"}, Healthy: false})
	r.Register(&LocalEndpoint{ID: "b", URL: "http://b", Provider: "ollama", Models: []string{"m1"}, Healthy: true})
	for i := 0; i < 4; i++ {
		ep, err := r.SelectEndpoint("m1", StrategyRoundRobin)
		if err != nil {
			t.Fatalf("SelectEndpoint: %v", err)
		}
		if ep.ID != "b" {
			t.Fatalf("unhealthy endpoint selected: %q", ep.ID)
		}
	}
}

func TestSelectEndpoint_NoHealthyError(t *testing.T) {
	r := NewRouter(nil)
	r.Register(&LocalEndpoint{ID: "a", URL: "http://a", Provider: "ollama", Models: []string{"m1"}, Healthy: false})
	_, err := r.SelectEndpoint("m1", StrategyRoundRobin)
	if !errors.Is(err, ErrNoHealthyEndpoint) {
		t.Fatalf("expected ErrNoHealthyEndpoint, got %v", err)
	}
}

func TestSelectEndpoint_PriorityPicksLowest(t *testing.T) {
	r := NewRouter(nil)
	r.Register(&LocalEndpoint{ID: "high", Provider: "ollama", Models: []string{"m"}, Healthy: true, Priority: 5})
	r.Register(&LocalEndpoint{ID: "low", Provider: "ollama", Models: []string{"m"}, Healthy: true, Priority: 1})
	ep, err := r.SelectEndpoint("m", StrategyPriority)
	if err != nil {
		t.Fatalf("SelectEndpoint: %v", err)
	}
	if ep.ID != "low" {
		t.Fatalf("expected 'low', got %q", ep.ID)
	}
}

func TestSelectEndpoint_LowestLatency(t *testing.T) {
	r := NewRouter(nil)
	r.Register(&LocalEndpoint{ID: "slow", Provider: "ollama", Models: []string{"m"}, Healthy: true, AvgLatencyMs: 500})
	r.Register(&LocalEndpoint{ID: "fast", Provider: "ollama", Models: []string{"m"}, Healthy: true, AvgLatencyMs: 80})
	ep, err := r.SelectEndpoint("m", StrategyLowestLatency)
	if err != nil {
		t.Fatalf("SelectEndpoint: %v", err)
	}
	if ep.ID != "fast" {
		t.Fatalf("expected 'fast', got %q", ep.ID)
	}
}

// ─── ShouldRouteLocally ──────────────────────────

func TestShouldRouteLocally_NoEndpoints(t *testing.T) {
	r := NewRouter(nil)
	if r.ShouldRouteLocally("m", 1000, false) {
		t.Fatal("expected false with no endpoints")
	}
}

func TestShouldRouteLocally_RespectsLatencyCap(t *testing.T) {
	r := NewRouter(nil)
	r.Register(&LocalEndpoint{ID: "a", Provider: "ollama", Models: []string{"m"}, Healthy: true, AvgLatencyMs: 1500})
	if r.ShouldRouteLocally("m", 1000, false) {
		t.Fatal("expected false when AvgLatencyMs > cap")
	}
	if !r.ShouldRouteLocally("m", 2000, false) {
		t.Fatal("expected true when AvgLatencyMs < cap")
	}
}

func TestShouldRouteLocally_RequireLocalOverrides(t *testing.T) {
	r := NewRouter(nil)
	// No endpoints, but requireLocal=true forces a true result.
	if !r.ShouldRouteLocally("m", 0, true) {
		t.Fatal("requireLocal should force true even without endpoints")
	}
}

// ─── RecordRequest ───────────────────────────────

func TestRecordRequest_EMA(t *testing.T) {
	r := NewRouter(nil)
	r.Register(&LocalEndpoint{ID: "a", Provider: "ollama", Models: []string{"m"}, Healthy: true})

	// First sample seeds the EMA exactly.
	r.RecordRequest("a", 100, true)
	got := r.Get("a")
	if got.AvgLatencyMs != 100 {
		t.Fatalf("expected 100 after first sample, got %d", got.AvgLatencyMs)
	}
	if got.ErrorRate != 0 {
		t.Fatalf("expected 0 error rate, got %f", got.ErrorRate)
	}

	// Second sample: 100 * 0.8 + 200 * 0.2 = 120
	r.RecordRequest("a", 200, true)
	got = r.Get("a")
	if got.AvgLatencyMs != 120 {
		t.Fatalf("expected 120 after second sample (EMA 0.2), got %d", got.AvgLatencyMs)
	}

	// A failure ticks the error EMA: 0 * 0.9 + 1 * 0.1 = 0.1
	r.RecordRequest("a", 200, false)
	got = r.Get("a")
	if got.ErrorRate < 0.099 || got.ErrorRate > 0.101 {
		t.Fatalf("expected ~0.1 error rate, got %f", got.ErrorRate)
	}
}

// ─── CheckHealth ─────────────────────────────────

func TestCheckHealth_MarksUnhealthyOnFailure(t *testing.T) {
	// httptest.Server that returns 500 simulates an endpoint
	// that is reachable but unhealthy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewRouter(nil)
	ep := &LocalEndpoint{
		ID: "u", URL: srv.URL, Provider: "ollama", Models: []string{"m"},
		Healthy: true, // starts healthy — we want the probe to flip it
	}
	r.Register(ep)

	healthy := r.CheckHealth(context.Background(), ep)
	if healthy {
		t.Fatal("expected unhealthy after 500 response")
	}
	if ep.Healthy {
		t.Fatal("endpoint.Healthy should be false")
	}
	if ep.LastCheckAt.IsZero() {
		t.Fatal("LastCheckAt should be set")
	}
}

func TestCheckHealth_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	r := NewRouter(nil)
	ep := &LocalEndpoint{
		ID: "h", URL: srv.URL, Provider: "ollama", Models: []string{"m"},
	}
	r.Register(ep)
	if !r.CheckHealth(context.Background(), ep) {
		t.Fatal("expected healthy")
	}
}

// ─── IncActive / DecActive feed Strategy=LeastLoaded ──

func TestSelectEndpoint_LeastLoaded(t *testing.T) {
	r := NewRouter(nil)
	r.Register(&LocalEndpoint{ID: "busy", Provider: "ollama", Models: []string{"m"}, Healthy: true})
	r.Register(&LocalEndpoint{ID: "idle", Provider: "ollama", Models: []string{"m"}, Healthy: true})
	r.IncActive("busy")
	r.IncActive("busy")
	ep, err := r.SelectEndpoint("m", StrategyLeastLoaded)
	if err != nil {
		t.Fatalf("SelectEndpoint: %v", err)
	}
	if ep.ID != "idle" {
		t.Fatalf("expected idle, got %q", ep.ID)
	}
}

// ─── StartHealthChecks runs async without blocking ────

func TestStartHealthChecks_DoesNotBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	r := NewRouter(nil)
	r.Register(&LocalEndpoint{ID: "a", URL: srv.URL, Provider: "ollama", Models: []string{"m"}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.StartHealthChecks(ctx, 50*time.Millisecond)
	// Give the initial sweep + at least one tick.
	time.Sleep(150 * time.Millisecond)
	got := r.Get("a")
	if !got.Healthy {
		t.Fatal("expected healthy after async sweep")
	}
}
