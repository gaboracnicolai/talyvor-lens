package api

// health.go — detailed /healthz handler. Replaces the boolean
// {"ok":true} probe with a real dependency rollup so an
// operator can tell at a glance which subsystem is degraded.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// HealthChecker is the contract the handler uses to query each
// subsystem. Returns (healthy, latencyMs, detail). The detail
// string surfaces partial failures (e.g. "1/3 endpoints
// unhealthy" for the local-model router).
type HealthChecker interface {
	Check(ctx context.Context) (healthy bool, latencyMs int64, detail string)
}

// HealthCheckFunc adapts a plain function to the HealthChecker
// interface. Most callers in main.go wire their dependencies
// with one of these.
type HealthCheckFunc func(ctx context.Context) (bool, int64, string)

func (f HealthCheckFunc) Check(ctx context.Context) (bool, int64, string) { return f(ctx) }

// HealthHandler runs every registered checker in parallel and
// rolls them up into the documented response shape.
type HealthHandler struct {
	version  string
	started  time.Time
	checkers map[string]HealthChecker
}

func NewHealthHandler(version string, checkers map[string]HealthChecker) *HealthHandler {
	return &HealthHandler{
		version:  version,
		started:  time.Now(),
		checkers: checkers,
	}
}

// ServeHTTP runs every checker in parallel with a 100ms budget
// per checker (so the overall response stays under 100ms as
// long as no checker hangs all by itself).
func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
	defer cancel()

	type result struct {
		name    string
		healthy bool
		latency int64
		detail  string
	}
	results := make([]result, 0, len(h.checkers))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for name, c := range h.checkers {
		wg.Add(1)
		go func(name string, c HealthChecker) {
			defer wg.Done()
			ok, lat, detail := c.Check(ctx)
			mu.Lock()
			results = append(results, result{name, ok, lat, detail})
			mu.Unlock()
		}(name, c)
	}
	wg.Wait()

	checks := map[string]any{}
	anyDown, anyDegraded := false, false
	for _, r := range results {
		entry := map[string]any{
			"status":     statusString(r.healthy, r.detail),
			"latency_ms": r.latency,
		}
		if r.detail != "" {
			entry["detail"] = r.detail
		}
		checks[r.name] = entry
		if !r.healthy {
			anyDown = true
		}
		if r.detail != "" && r.healthy {
			anyDegraded = true
		}
	}

	overall := "healthy"
	switch {
	case anyDown:
		overall = "unhealthy"
	case anyDegraded:
		overall = "degraded"
	}

	status := http.StatusOK
	if overall == "unhealthy" {
		// 503 keeps load balancers from sending traffic during
		// dependency outages.
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         overall,
		"version":        h.version,
		"uptime_seconds": int64(time.Since(h.started).Seconds()),
		"checks":         checks,
	})
}

func statusString(healthy bool, detail string) string {
	if !healthy {
		return "unhealthy"
	}
	if detail != "" {
		return "degraded"
	}
	return "healthy"
}
