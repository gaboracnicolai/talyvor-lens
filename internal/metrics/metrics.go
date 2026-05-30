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

	// ─── cost forecasting (Upgrade 20) ───
	// Both carry only the bounded {scope} label (workspace/team/sprint).
	// scope_id is deliberately OMITTED to keep cardinality bounded by
	// construction — same discipline as budgets, no unbounded id labels.
	ForecastProjectedUSD = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_forecast_projected_usd", Help: "Most recent projected period-total spend (USD), by scope. A projection, not actual spend."},
		[]string{"scope"},
	)
	ForecastWillExceedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_forecast_will_exceed_budget_total", Help: "Times a forecast projected a budget would be exceeded, by scope."},
		[]string{"scope"},
	)

	// ─── cost anomaly detection (Upgrade 21) ───
	// Bounded labels only: scope (issue/team/sprint) and severity
	// (low/warn/high). unit_id / issue_id are deliberately OMITTED — they're
	// unbounded, so they never become label values.
	AnomaliesDetectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_anomalies_detected_total", Help: "Cost anomalies detected (deduped per unit+period), by scope and severity."},
		[]string{"scope", "severity"},
	)
	AnomalyMaxFactor = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "lens_anomaly_max_factor", Help: "Largest cost factor over baseline median observed in the most recent scan, by scope."},
		[]string{"scope"},
	)

	// ─── executive ROI reporting (Upgrade 24) ───
	// Bounded label: period (monthly/weekly/total). No workspace/unit labels.
	ROIReportsGeneratedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_roi_report_generated_total", Help: "Executive ROI reports generated, by period."},
		[]string{"period"},
	)
	ROIReportDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{Name: "lens_roi_report_generation_duration_seconds", Help: "ROI report generation latency (an expensive aggregation).", Buckets: latencyBuckets},
	)

	// ─── routing intelligence (Upgrade 22) ───
	// Bounded label: basis (quality_per_dollar | quality | none). No
	// feature/model/workspace labels.
	RoutingRecommendationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_routing_recommendations_total", Help: "Routing recommendations computed on the request path, by basis."},
		[]string{"basis"},
	)
	RoutingIntelligenceAppliedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_routing_intelligence_applied_total", Help: "Requests whose model was changed from the default by routing intelligence."},
	)
	RoutingFallbackTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_routing_fallback_total", Help: "Requests that fell back to the default route (no recommendation / below sample floor)."},
	)

	// ─── vision / multimodal routing (Upgrade 15) ───
	// modality is a bounded, low-cardinality label (text / image / audio /
	// document / comma-joined sets). No per-model or per-workspace labels.
	RequestsByModalityTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "lens_requests_by_modality_total", Help: "Requests by detected modality."},
		[]string{"modality"},
	)
	VisionRouteRedirectsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_vision_route_redirects_total", Help: "Requests redirected to a modality-capable model (auto-route)."},
	)
	ModalityUnsupportedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{Name: "lens_modality_unsupported_total", Help: "Requests failed fast because no configured model supports the requested modality."},
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
		ForecastProjectedUSD, ForecastWillExceedTotal,
		AnomaliesDetectedTotal, AnomalyMaxFactor,
		ROIReportsGeneratedTotal, ROIReportDuration,
		RoutingRecommendationsTotal, RoutingIntelligenceAppliedTotal, RoutingFallbackTotal,
		RequestsByModalityTotal, VisionRouteRedirectsTotal, ModalityUnsupportedTotal,
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

// ─── cost forecasting helpers ───
// Bounded {scope} label only — no scope_id, so no cardinality guard needed.

func SetForecastProjected(scope string, v float64) {
	ForecastProjectedUSD.WithLabelValues(scope).Set(v)
}
func ForecastWillExceed(scope string) { ForecastWillExceedTotal.WithLabelValues(scope).Inc() }

// ─── cost anomaly helpers ───
// Bounded {scope, severity} labels only — no unit_id, so no guard needed.

func AnomalyDetected(scope, severity string) {
	AnomaliesDetectedTotal.WithLabelValues(scope, severity).Inc()
}
func SetAnomalyMaxFactor(scope string, v float64) {
	AnomalyMaxFactor.WithLabelValues(scope).Set(v)
}

// ─── ROI reporting helpers ───
// Bounded {period} label only.

func ROIReportGenerated(period string) { ROIReportsGeneratedTotal.WithLabelValues(period).Inc() }
func ObserveROIReportDuration(d time.Duration) {
	ROIReportDuration.Observe(d.Seconds())
}

// ─── routing intelligence helpers ───
// Bounded {basis} label on recommendations; applied/fallback are unlabeled.

func RoutingRecommendation(basis string) { RoutingRecommendationsTotal.WithLabelValues(basis).Inc() }
func RoutingIntelligenceApplied()        { RoutingIntelligenceAppliedTotal.Inc() }
func RoutingFallback()                   { RoutingFallbackTotal.Inc() }

// ─── vision / multimodal helpers ───
// Bounded {modality} label on the counter; redirects/unsupported unlabeled.

func RequestByModality(modality string) { RequestsByModalityTotal.WithLabelValues(modality).Inc() }
func VisionRouteRedirect()              { VisionRouteRedirectsTotal.Inc() }
func ModalityUnsupported()              { ModalityUnsupportedTotal.Inc() }
