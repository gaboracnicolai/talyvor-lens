package localrouter

// multi.go — multi-endpoint local-model Router. Coexists with
// the legacy `LocalRouter` (single Ollama instance) in router.go.
// New code paths should use Router; the proxy still wires the
// legacy LocalRouter for the simple single-Ollama deployment.
//
// Key responsibilities:
//   - Hold an in-memory registry of LocalEndpoints across
//     providers (Ollama / vLLM / llamacpp).
//   - Run async health checks (every 30s) so the request hot
//     path only reads cached health, never blocks.
//   - Select an endpoint per request using one of four strategies
//     (round-robin / least-loaded / lowest-latency / priority).
//   - Track per-endpoint EMA latency + error rate so the
//     least-loaded / lowest-latency strategies have real data.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── constants ───────────────────────────────────

const (
	// DefaultHealthCheckInterval is the cadence for StartHealthChecks.
	// 30s mirrors the legacy LocalRouter — frequent enough that an
	// endpoint going down is reflected in routing within one window,
	// rare enough that we don't hammer hobby-grade local servers.
	DefaultHealthCheckInterval = 30 * time.Second

	// healthCheckTimeout caps a single endpoint probe. Local
	// servers should answer /api/tags or /v1/models in <100ms.
	healthCheckTimeout = 3 * time.Second

	// latencyEMAWeight controls how fast AvgLatencyMs reacts to
	// new samples. 0.2 = "the latest call counts for 20% of the
	// new average". Spec-mandated.
	latencyEMAWeight = 0.2

	// errorRateEMAWeight is intentionally smaller (0.1 — slow
	// decay) so one transient blip doesn't yank the error rate.
	errorRateEMAWeight = 0.1
)

// ─── types ───────────────────────────────────────

// RoutingStrategy is the selection policy SelectEndpoint applies
// when more than one healthy endpoint can serve a model.
type RoutingStrategy string

const (
	StrategyRoundRobin    RoutingStrategy = "round_robin"
	StrategyLeastLoaded   RoutingStrategy = "least_loaded"
	StrategyLowestLatency RoutingStrategy = "lowest_latency"
	StrategyPriority      RoutingStrategy = "priority"
)

// LocalEndpoint is one configured local-model server. Mutable
// fields (Healthy / LastCheckAt / EMA stats / active count) are
// guarded by Router.mu — never touched outside the router.
type LocalEndpoint struct {
	ID            string    `json:"id"`
	WorkspaceID   string    `json:"workspace_id,omitempty"` // mining ownership (Batch 2 Item 2)
	URL           string    `json:"url"`
	Provider      string    `json:"provider"`
	Models        []string  `json:"models"`
	Priority      int       `json:"priority"`
	MaxConcurrent int       `json:"max_concurrent"`
	Active        bool      `json:"active"`
	Healthy       bool      `json:"healthy"`
	LastCheckAt   time.Time `json:"last_check_at"`
	AvgLatencyMs  int64     `json:"avg_latency_ms"`
	ErrorRate     float64   `json:"error_rate"`

	// activeCount tracks in-flight requests. Atomic so
	// SelectEndpoint can read it under the router's RLock
	// without taking the count.
	activeCount int64 `json:"-"`
}

// Router is the multi-endpoint registry + selector.
type Router struct {
	endpoints  []*LocalEndpoint
	mu         sync.RWMutex
	httpClient *http.Client
	pool       *pgxpool.Pool

	// rrCursor is the round-robin index. Atomic so we don't
	// take a write lock just to advance the counter.
	rrCursor uint64

	// onRequestServed is the compute-mining hook the proxy
	// invokes after a successful served request. Stays nil
	// until SetOnRequestServed wires it (Batch 2 Item 2).
	onRequestServed func(nodeID, requestingWorkspace string, tokens int, latencyMs int64)
}

// SetOnRequestServed installs the mining hook. main.go wires it
// to ComputeMiner.RecordServedRequest. The hook fires from
// NotifyServed below — keeping the router free of a hard
// dependency on the mining package.
func (r *Router) SetOnRequestServed(fn func(nodeID, requestingWorkspace string, tokens int, latencyMs int64)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRequestServed = fn
}

// NotifyServed is the proxy-side entry point: "this endpoint
// just served `tokens` tokens to `requestingWorkspace` in
// `latencyMs`". When a mining hook is wired it's invoked
// synchronously so the proxy can `defer` it and not lose the
// accounting on early returns.
func (r *Router) NotifyServed(nodeID, requestingWorkspace string, tokens int, latencyMs int64) {
	r.mu.RLock()
	fn := r.onRequestServed
	r.mu.RUnlock()
	if fn != nil {
		fn(nodeID, requestingWorkspace, tokens, latencyMs)
	}
}

// ─── errors ──────────────────────────────────────

var (
	ErrNoHealthyEndpoint = errors.New("localrouter: no healthy endpoint for model")
	ErrUnknownStrategy   = errors.New("localrouter: unknown routing strategy")
)

// ─── constructors ────────────────────────────────

// NewRouter builds an empty Router. Endpoints get added via
// Register() or via ParseEndpointsConfig + NewRouterFromConfig.
func NewRouter(pool *pgxpool.Pool) *Router {
	return &Router{
		httpClient: &http.Client{Timeout: healthCheckTimeout},
		pool:       pool,
	}
}

// NewRouterFromConfig is the env-driven constructor. It calls
// ParseEndpointsConfig and registers each result. Malformed
// entries are logged via the caller (we surface them via
// (results, err) only when *all* entries fail).
func NewRouterFromConfig(pool *pgxpool.Pool, configStr string) *Router {
	r := NewRouter(pool)
	endpoints, _ := ParseEndpointsConfig(configStr)
	for _, e := range endpoints {
		r.Register(e)
	}
	return r
}

// ─── registry ────────────────────────────────────

// Register adds an endpoint. New endpoints start with
// Active=true, Healthy=false — the first health check will flip
// them. Idempotent on ID: re-registering replaces the slot.
func (r *Router) Register(e *LocalEndpoint) {
	if e == nil {
		return
	}
	if e.ID == "" {
		e.ID = fmt.Sprintf("%s-%d", e.Provider, time.Now().UnixNano())
	}
	if e.MaxConcurrent <= 0 {
		e.MaxConcurrent = 16
	}
	e.URL = strings.TrimRight(e.URL, "/")
	e.Active = true
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, ex := range r.endpoints {
		if ex.ID == e.ID {
			r.endpoints[i] = e
			return
		}
	}
	r.endpoints = append(r.endpoints, e)
}

// Remove deletes an endpoint by ID. Returns false when no such
// ID exists — callers use that to surface a 404.
func (r *Router) Remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.endpoints {
		if e.ID == id {
			r.endpoints = append(r.endpoints[:i], r.endpoints[i+1:]...)
			return true
		}
	}
	return false
}

// Get returns a deep-copy of one endpoint for read-only callers
// (admin API). Returns nil if the ID is unknown.
func (r *Router) Get(id string) *LocalEndpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.endpoints {
		if e.ID == id {
			cp := *e
			cp.Models = append([]string{}, e.Models...)
			return &cp
		}
	}
	return nil
}

// List returns a snapshot of all endpoints. Safe to expose
// directly via the admin API.
func (r *Router) List() []*LocalEndpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*LocalEndpoint, 0, len(r.endpoints))
	for _, e := range r.endpoints {
		cp := *e
		cp.Models = append([]string{}, e.Models...)
		out = append(out, &cp)
	}
	return out
}

// ─── health checks ───────────────────────────────

// CheckHealth probes a single endpoint and flips its Healthy
// field. Returns the final state for the caller (so the admin
// "check now" endpoint can return it inline).
func (r *Router) CheckHealth(ctx context.Context, endpoint *LocalEndpoint) bool {
	if endpoint == nil {
		return false
	}
	healthy := r.probe(ctx, endpoint)
	r.mu.Lock()
	endpoint.Healthy = healthy
	endpoint.LastCheckAt = time.Now()
	r.mu.Unlock()
	return healthy
}

// probe runs the provider-specific health request. Tests
// override the httpClient with one wired to httptest.Server.
func (r *Router) probe(ctx context.Context, endpoint *LocalEndpoint) bool {
	path := "/health"
	switch endpoint.Provider {
	case "ollama":
		path = "/api/tags"
	case "vllm":
		path = "/v1/models"
	case "llamacpp":
		path = "/health"
	}
	reqCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint.URL+path, nil)
	if err != nil {
		return false
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	// Best-effort consume — we don't care about the body for
	// llamacpp/vllm. For ollama we *could* parse /api/tags here,
	// but the spec says "model available" is satisfied by the
	// configured Models list, so we just confirm the endpoint
	// itself is up.
	_, _ = io.Copy(io.Discard, resp.Body)
	return true
}

// CheckHealthByID looks up the live endpoint by ID (not a copy)
// and probes it. Returns ErrNoHealthyEndpoint when the ID is
// unknown — admin "check now" uses this so the registry state
// reflects the fresh probe.
func (r *Router) CheckHealthByID(ctx context.Context, id string) (*LocalEndpoint, error) {
	r.mu.RLock()
	var target *LocalEndpoint
	for _, e := range r.endpoints {
		if e.ID == id {
			target = e
			break
		}
	}
	r.mu.RUnlock()
	if target == nil {
		return nil, ErrNoHealthyEndpoint
	}
	r.CheckHealth(ctx, target)
	return r.Get(id), nil
}

// StartHealthChecks runs CheckHealth on every endpoint at the
// given interval. Returns immediately — caller is responsible
// for cancelling ctx to stop the loop. Health checks are async
// so they never block requests.
func (r *Router) StartHealthChecks(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHealthCheckInterval
	}
	go func() {
		// Initial sweep so the first request after startup sees
		// a populated health state.
		r.sweepHealth(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.sweepHealth(ctx)
			}
		}
	}()
}

func (r *Router) sweepHealth(ctx context.Context) {
	for _, e := range r.List() { // List() returns copies — safe
		// We can't pass the copy to CheckHealth because we want
		// to mutate the original. Look up the real one by ID.
		r.mu.RLock()
		var target *LocalEndpoint
		for _, real := range r.endpoints {
			if real.ID == e.ID {
				target = real
				break
			}
		}
		r.mu.RUnlock()
		if target != nil {
			r.CheckHealth(ctx, target)
		}
	}
}

// ─── selection ───────────────────────────────────

// SelectEndpoint picks one healthy, active endpoint that serves
// `model`. Returns ErrNoHealthyEndpoint when nothing matches.
// Never returns silently — if zero healthy endpoints, the
// caller gets an explicit error and can fall back to cloud.
func (r *Router) SelectEndpoint(model string, strategy RoutingStrategy) (*LocalEndpoint, error) {
	r.mu.RLock()
	candidates := make([]*LocalEndpoint, 0, len(r.endpoints))
	for _, e := range r.endpoints {
		if !e.Active || !e.Healthy {
			continue
		}
		if !servesModel(e, model) {
			continue
		}
		candidates = append(candidates, e)
	}
	r.mu.RUnlock()

	if len(candidates) == 0 {
		return nil, ErrNoHealthyEndpoint
	}

	switch strategy {
	case StrategyRoundRobin, "":
		idx := atomic.AddUint64(&r.rrCursor, 1)
		return candidates[(int(idx)-1)%len(candidates)], nil
	case StrategyLeastLoaded:
		best := candidates[0]
		bestN := atomic.LoadInt64(&best.activeCount)
		for _, c := range candidates[1:] {
			n := atomic.LoadInt64(&c.activeCount)
			if n < bestN {
				best, bestN = c, n
			}
		}
		return best, nil
	case StrategyLowestLatency:
		best := candidates[0]
		for _, c := range candidates[1:] {
			// Endpoints that have never been measured (Avg=0)
			// lose to ones that have data; otherwise pick the
			// lowest measurement.
			if best.AvgLatencyMs == 0 && c.AvgLatencyMs > 0 {
				best = c
				continue
			}
			if c.AvgLatencyMs > 0 && c.AvgLatencyMs < best.AvgLatencyMs {
				best = c
			}
		}
		return best, nil
	case StrategyPriority:
		best := candidates[0]
		for _, c := range candidates[1:] {
			if c.Priority < best.Priority {
				best = c
			}
		}
		return best, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownStrategy, strategy)
	}
}

func servesModel(e *LocalEndpoint, model string) bool {
	if len(e.Models) == 0 {
		// Empty Models list = "serves anything". Useful for
		// generic local proxies; risky for shared deployments,
		// so callers should set Models explicitly.
		return true
	}
	for _, m := range e.Models {
		if m == model || strings.HasPrefix(model, m) {
			return true
		}
	}
	return false
}

// ─── policy decision ─────────────────────────────

// ShouldRouteLocally is the "decide before dialing" check the
// proxy calls. requireLocal forces a true answer (the caller
// has already decided); otherwise we require a healthy endpoint
// *and* an average latency under maxLatencyMs (0 = no cap).
func (r *Router) ShouldRouteLocally(model string, maxLatencyMs int, requireLocal bool) bool {
	if requireLocal {
		return true
	}
	ep, err := r.SelectEndpoint(model, StrategyLowestLatency)
	if err != nil {
		return false
	}
	if maxLatencyMs <= 0 {
		return true
	}
	// An endpoint that's never been measured has AvgLatencyMs=0
	// — treat that as "unknown but allowed" so the first call
	// still routes locally and seeds the EMA.
	if ep.AvgLatencyMs == 0 {
		return true
	}
	return ep.AvgLatencyMs < int64(maxLatencyMs)
}

// ─── request tracking ────────────────────────────

// RecordRequest folds one observation into the endpoint's EMA
// stats. latencyMs may be 0 for a connection-refused error.
func (r *Router) RecordRequest(endpointID string, latencyMs int64, success bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.endpoints {
		if e.ID != endpointID {
			continue
		}
		if e.AvgLatencyMs == 0 {
			e.AvgLatencyMs = latencyMs
		} else {
			e.AvgLatencyMs = int64(float64(e.AvgLatencyMs)*(1-latencyEMAWeight) +
				float64(latencyMs)*latencyEMAWeight)
		}
		var sampleErr float64
		if !success {
			sampleErr = 1.0
		}
		e.ErrorRate = e.ErrorRate*(1-errorRateEMAWeight) + sampleErr*errorRateEMAWeight
		return
	}
}

// IncActive / DecActive are called by the proxy around an
// outgoing call so SelectEndpoint(StrategyLeastLoaded) has data.
func (r *Router) IncActive(endpointID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.endpoints {
		if e.ID == endpointID {
			atomic.AddInt64(&e.activeCount, 1)
			return
		}
	}
}

func (r *Router) DecActive(endpointID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.endpoints {
		if e.ID == endpointID {
			atomic.AddInt64(&e.activeCount, -1)
			return
		}
	}
}

// ─── config parsing ──────────────────────────────

// ParseEndpointsConfig parses LENS_LOCAL_ENDPOINTS-style strings:
//
//	provider:url:model1,model2;provider:url:model3
//
// Lenient by design — malformed segments are skipped and counted
// in the returned error (callers can log it; an empty config
// returns nil, nil). A returned slice + non-nil error means
// "we got some endpoints, but some entries were bad".
func ParseEndpointsConfig(config string) ([]*LocalEndpoint, error) {
	if strings.TrimSpace(config) == "" {
		return nil, nil
	}
	var out []*LocalEndpoint
	var skipped []string
	for _, entry := range strings.Split(config, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Split on ":" but URLs contain colons too — so we treat
		// the LAST piece as models, the FIRST as provider, and
		// reassemble everything in the middle as the URL.
		parts := strings.Split(entry, ":")
		if len(parts) < 3 {
			skipped = append(skipped, entry)
			continue
		}
		provider := parts[0]
		models := parts[len(parts)-1]
		url := strings.Join(parts[1:len(parts)-1], ":")
		if provider == "" || url == "" || models == "" {
			skipped = append(skipped, entry)
			continue
		}
		switch provider {
		case "ollama", "vllm", "llamacpp":
			// supported
		default:
			skipped = append(skipped, entry)
			continue
		}
		modelList := []string{}
		for _, m := range strings.Split(models, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				modelList = append(modelList, m)
			}
		}
		if len(modelList) == 0 {
			skipped = append(skipped, entry)
			continue
		}
		out = append(out, &LocalEndpoint{
			Provider:      provider,
			URL:           strings.TrimRight(url, "/"),
			Models:        modelList,
			Active:        true,
			MaxConcurrent: 16,
		})
	}
	if len(skipped) > 0 {
		return out, fmt.Errorf("localrouter: skipped %d malformed entries: %s",
			len(skipped), strings.Join(skipped, " | "))
	}
	return out, nil
}

// ─── small helpers used by api server ────────────

// EndpointsJSON is the admin-API shape: same as LocalEndpoint
// but with the unexported activeCount surfaced as ActiveCount.
type EndpointsJSON struct {
	*LocalEndpoint
	ActiveCount int64 `json:"active_count"`
}

// MarshalJSON folds activeCount into the JSON output without
// changing the struct definition.
func (e *LocalEndpoint) MarshalJSON() ([]byte, error) {
	type alias LocalEndpoint
	return json.Marshal(&struct {
		*alias
		ActiveCount int64 `json:"active_count"`
	}{
		alias:       (*alias)(e),
		ActiveCount: atomic.LoadInt64(&e.activeCount),
	})
}
