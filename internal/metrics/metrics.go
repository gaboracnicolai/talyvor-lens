// Package metrics owns every Prometheus metric Lens exposes. One registry
// (the client_golang default), one place to define names + labels, so
// cardinality stays under control. All metric names use the "lens_" namespace
// prefix.
//
// Cardinality discipline: labels are bounded sets only — route PATTERNS (not
// raw paths), provider names, cache layers, normalized status codes, breaker
// providers, and the (bounded) workspace tenant set. Never request IDs or any
// unbounded value.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// latencyBuckets covers sub-millisecond to multi-second HTTP/upstream latency.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// ledgerBuckets is tuned for the token-ledger write hot path (sub-ms to ~1s) so
// the 100ms p99 alert has resolution around its threshold.
var ledgerBuckets = []float64{0.0001, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}

var (
	// ─── pre-existing (kept; do not rename) ───
	RequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_requests_total", Help: "Total proxied requests, by provider and outcome."},
		[]string{"provider", "outcome"},
	)
	CacheHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_cache_hits_total", Help: "Cache hits, by cache layer."},
		[]string{"layer"},
	)
	TokensSavedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_tokens_saved_total", Help: "Tokens saved, by provider and strategy."},
		[]string{"provider", "strategy"},
	)

	// ─── HTTP layer (Upgrade 11) ───
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_http_requests_total", Help: "HTTP requests, by route pattern, method, and status code."},
		[]string{"route", "method", "status"},
	)
	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "lens_http_request_duration_seconds", Help: "HTTP request latency, by route and method.", Buckets: latencyBuckets},
		[]string{"route", "method"},
	)
	InflightRequests = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "lens_inflight_requests", Help: "In-flight HTTP requests currently being served."},
	)

	// ─── upstream providers ───
	UpstreamRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_upstream_provider_requests_total", Help: "Upstream provider requests, by provider and status."},
		[]string{"provider", "status"},
	)
	UpstreamDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "lens_upstream_provider_duration_seconds", Help: "Upstream provider call latency, by provider.", Buckets: latencyBuckets},
		[]string{"provider"},
	)

	// ─── cache ───
	CacheMissesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_cache_misses_total", Help: "Cache misses, by cache layer."},
		[]string{"layer"},
	)

	// ─── circuit breaker ───
	CircuitBreakerState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_circuit_breaker_state", Help: "Circuit breaker state by provider: 0=closed, 1=open, 2=half-open."},
		[]string{"provider"},
	)

	// ─── rate limiting ───
	// NOTE: workspace is a bounded tenant set; if a deployment has unbounded
	// workspaces, normalize this label (e.g. bucket unknown → "other").
	RateLimitRejectionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_rate_limit_rejections_total", Help: "Rate-limit rejections (HTTP 429), by workspace."},
		[]string{"workspace"},
	)

	// ─── token ledger hot path ───
	TokenLedgerWriteDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{Name: "lens_token_ledger_write_duration_seconds", Help: "Token-ledger write latency (the minting hot path).", Buckets: ledgerBuckets},
	)

	// ─── economy ───
	TokensMintedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_tokens_minted_total", Help: "Total LENS tokens minted."},
	)
	LXCConvertedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_lxc_converted_total", Help: "Total LXC minted via LENS→LXC conversion."},
	)

	// ─── high availability ───
	HAInstanceCount = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "lens_ha_instance_count", Help: "Active Lens instances seen in the HA registry (0 when HA disabled)."},
	)

	// ─── budget governance (Upgrade 19) ───
	// scope is bounded (workspace/team/sprint). scope_id is potentially
	// unbounded (team / sprint ids), so the budgets service gates these two
	// gauges behind a cardinality guard before calling the setters below —
	// never emit a scope_id label without that guard. The two counters use
	// only the bounded {scope} label and are safe to emit unguarded.
	BudgetSpentUSD = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_budget_spent_usd", Help: "Current spend (USD) per budget, by scope and scope_id."},
		[]string{"scope", "scope_id"},
	)
	BudgetUtilizationRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_budget_utilization_ratio", Help: "Spend / limit ratio per budget, by scope and scope_id."},
		[]string{"scope", "scope_id"},
	)
	BudgetThresholdCrossedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_budget_threshold_crossed_total", Help: "Alert thresholds crossed, by scope."},
		[]string{"scope"},
	)
	BudgetBlocksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_budget_blocks_total", Help: "Requests blocked by a hard_block budget, by scope."},
		[]string{"scope"},
	)
)

func init() {
	prometheus.MustRegister(
		RequestsTotal, CacheHitsTotal, TokensSavedTotal,
		HTTPRequestsTotal, HTTPRequestDuration, InflightRequests,
		UpstreamRequestsTotal, UpstreamDuration,
		CacheMissesTotal,
		CircuitBreakerState,
		RateLimitRejectionsTotal,
		TokenLedgerWriteDuration,
		TokensMintedTotal, LXCConvertedTotal,
		HAInstanceCount,
		BudgetSpentUSD, BudgetUtilizationRatio,
		BudgetThresholdCrossedTotal, BudgetBlocksTotal,
	)
}

func Handler() http.Handler { return promhttp.Handler() }

// ─── thin helpers (keep call sites one-liners) ───

// RecordUpstream records an upstream provider call's outcome + latency.
func RecordUpstream(provider, status string, dur time.Duration) {
	UpstreamRequestsTotal.WithLabelValues(provider, status).Inc()
	UpstreamDuration.WithLabelValues(provider).Observe(dur.Seconds())
}

// RecordCacheHit / RecordCacheMiss track cache effectiveness by layer.
func RecordCacheHit(layer string)  { CacheHitsTotal.WithLabelValues(layer).Inc() }
func RecordCacheMiss(layer string) { CacheMissesTotal.WithLabelValues(layer).Inc() }

// SetBreakerState records a breaker transition (0 closed, 1 open, 2 half-open).
func SetBreakerState(provider string, state float64) {
	CircuitBreakerState.WithLabelValues(provider).Set(state)
}

// RateLimitRejected increments the per-workspace rejection counter.
func RateLimitRejected(workspace string) {
	if workspace == "" {
		workspace = "unknown"
	}
	RateLimitRejectionsTotal.WithLabelValues(workspace).Inc()
}

// ObserveLedgerWrite records a token-ledger write duration.
func ObserveLedgerWrite(dur time.Duration) { TokenLedgerWriteDuration.Observe(dur.Seconds()) }

// MintedTokens / ConvertedLXC bump the economy counters.
func MintedTokens(n float64) {
	if n > 0 {
		TokensMintedTotal.Add(n)
	}
}
func ConvertedLXC(n float64) {
	if n > 0 {
		LXCConvertedTotal.Add(n)
	}
}

// SetHAInstanceCount publishes the current active-instance count.
func SetHAInstanceCount(n int) { HAInstanceCount.Set(float64(n)) }

// ─── budget governance helpers ───
// SetBudgetSpent / SetBudgetUtilization carry the unbounded scope_id label;
// callers MUST apply the budgets cardinality guard before invoking them.

func SetBudgetSpent(scope, scopeID string, v float64) {
	BudgetSpentUSD.WithLabelValues(scope, scopeID).Set(v)
}

func SetBudgetUtilization(scope, scopeID string, v float64) {
	BudgetUtilizationRatio.WithLabelValues(scope, scopeID).Set(v)
}

// BudgetThresholdCrossed / BudgetBlocked use only the bounded {scope} label.
func BudgetThresholdCrossed(scope string) { BudgetThresholdCrossedTotal.WithLabelValues(scope).Inc() }
func BudgetBlocked(scope string)          { BudgetBlocksTotal.WithLabelValues(scope).Inc() }
