package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/anomaly"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/costanomaly"
	"github.com/talyvor/lens/internal/eval"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/forecast"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/roi"
	"github.com/talyvor/lens/internal/routing"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/workspace"
)

// pgxDB is the subset of *pgxpool.Pool the API server needs. The Ping
// method is for the health endpoint; the other three for analytics.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Ping(ctx context.Context) error
}

// Analyser is the interface the recommendations endpoint needs. *learner.Learner
// satisfies it; tests use a stub so they don't have to drive the learner's
// internal pool.
type Analyser interface {
	Analyse(ctx context.Context) ([]learner.PatternInsight, error)
}

type Server struct {
	pool             pgxDB
	redisClient      *redis.Client
	natsConn         *nats.Conn
	exactCache       *cache.ExactCache
	analyser         Analyser
	alertManager     *alerts.AlertManager
	tracker          *attribution.Tracker
	wsManager        *workspace.Manager
	localRouter      *localrouter.LocalRouter
	anomalyDetector  *anomaly.Detector
	budgetStore      *budgets.Store
	forecaster       *forecast.Forecaster
	costAnomaly      *costanomaly.Detector
	roiReporter      *roi.Reporter
	routingAdvisor   *routing.Advisor
	guardrails       *guardrails.Engine
	evalPipeline     *eval.Pipeline
	poviStakes       *povi.StakeManager
	poviChallenges   *povi.ChallengeStore
	version          string
	startTime        time.Time
}

// serverDeps is the test-friendly constructor input. Public NewServer
// translates *pgxpool.Pool + *learner.Learner into the interface fields so
// the typed-nil interface trap can't bite.
type serverDeps struct {
	pool             pgxDB
	redisClient      *redis.Client
	natsConn         *nats.Conn
	exactCache       *cache.ExactCache
	analyser         Analyser
	alertManager     *alerts.AlertManager
	tracker          *attribution.Tracker
	wsManager        *workspace.Manager
	localRouter      *localrouter.LocalRouter
	anomalyDetector  *anomaly.Detector
	version          string
	startTime        time.Time
}

func newServer(d serverDeps) *Server {
	if d.startTime.IsZero() {
		d.startTime = time.Now()
	}
	if d.version == "" {
		d.version = "dev"
	}
	return &Server{
		pool:            d.pool,
		redisClient:     d.redisClient,
		natsConn:        d.natsConn,
		exactCache:      d.exactCache,
		analyser:        d.analyser,
		alertManager:    d.alertManager,
		tracker:         d.tracker,
		wsManager:       d.wsManager,
		localRouter:     d.localRouter,
		anomalyDetector: d.anomalyDetector,
		version:         d.version,
		startTime:       d.startTime,
	}
}

func NewServer(
	pool *pgxpool.Pool,
	redisClient *redis.Client,
	natsConn *nats.Conn,
	exactCache *cache.ExactCache,
	learnerImpl *learner.Learner,
	alertManager *alerts.AlertManager,
	tracker *attribution.Tracker,
	wsManager *workspace.Manager,
	localRouter *localrouter.LocalRouter,
	anomalyDetector *anomaly.Detector,
	version string,
) *Server {
	var poolI pgxDB
	if pool != nil {
		poolI = pool
	}
	var analyser Analyser
	if learnerImpl != nil {
		analyser = learnerImpl
	}
	return newServer(serverDeps{
		pool:            poolI,
		redisClient:     redisClient,
		natsConn:        natsConn,
		exactCache:      exactCache,
		analyser:        analyser,
		alertManager:    alertManager,
		tracker:         tracker,
		wsManager:       wsManager,
		localRouter:     localRouter,
		anomalyDetector: anomalyDetector,
		version:         version,
		startTime:       time.Now(),
	})
}

// Mount registers every API route on r. Tests and simple setups use this;
// production mounts the auth-gated subset under an authenticated chi group
// and the public subset on the bare router via MountAuthenticated /
// MountUnauthenticated.
func (s *Server) Mount(r chi.Router) {
	s.MountUnauthenticated(r)
	s.MountAuthenticated(r)
}

// MountUnauthenticated registers the routes that must NOT require an API
// key: health probe and the Prometheus metrics passthrough.
func (s *Server) MountUnauthenticated(r chi.Router) {
	r.Get("/v1/api/health", s.handleHealth)
	r.Handle("/v1/api/metrics/prometheus", metrics.Handler())
}

// MountAuthenticated registers the routes that should sit behind
// AuthMiddleware: all analytics, model, workspace, and alerts endpoints.
func (s *Server) MountAuthenticated(r chi.Router) {
	r.Get("/v1/api/spend/summary", s.handleSpendSummary)
	r.Get("/v1/api/spend/by-model", s.handleSpendBy("model"))
	r.Get("/v1/api/spend/by-team", s.handleSpendBy("team"))
	r.Get("/v1/api/spend/by-feature", s.handleSpendBy("feature"))
	r.Get("/v1/api/cache/stats", s.handleCacheStats)
	r.Get("/v1/api/cache/top-patterns", s.handleCacheTopPatterns)
	r.Get("/v1/api/models/usage", s.handleSpendBy("model"))
	r.Get("/v1/api/models/recommendations", s.handleModelsRecommendations)
	r.Get("/v1/api/eval/runs", s.handleEvalRuns)
	r.Get("/v1/api/povi/stakes", s.handlePOVIStakes)
	r.Get("/v1/api/povi/challenges", s.handlePOVIChallenges)
	r.Get("/v1/api/workspaces", s.handleWorkspaces)
	r.Get("/v1/api/alerts/circuits", s.handleAlertsCircuits)
	r.Get("/v1/api/alerts/rules", s.handleAlertsRules)
	r.Get("/v1/api/local/status", s.handleLocalStatus)
	r.Get("/v1/api/anomalies", s.handleAnomalies)
	r.Get("/v1/api/anomalies/scan", s.handleAnomaliesScan)
	r.Get("/v1/api/budgets", s.handleBudgets)
	r.Get("/v1/api/forecast/summary", s.handleForecastSummary)
	r.Get("/v1/api/costanomalies", s.handleCostAnomalies)
	r.Get("/v1/api/roi/summary", s.handleROISummary)
	r.Get("/v1/api/distill/summary", s.handleDistillSummary)
	r.Get("/v1/api/routing/intelligence", s.handleRoutingIntelligence)
	r.Get("/v1/api/modality/capabilities", s.handleModalityCapabilities)
	r.Get("/v1/api/guardrails", s.handleGuardrails)
	r.Get("/v1/api/catalog", s.handleCatalog)
}

// handleCatalog returns the full model catalog for the dashboard. Static
// (read-mostly), no state.
func (s *Server) handleCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, catalog.All())
}

// SetGuardrailsEngine wires the guardrails engine for the dashboard panel.
func (s *Server) SetGuardrailsEngine(e *guardrails.Engine) { s.guardrails = e }

// handleGuardrails returns the default workspace's policy + whether the
// output stage is enabled, for the dashboard. Hidden client-side when the
// output stage is off.
func (s *Server) handleGuardrails(w http.ResponseWriter, r *http.Request) {
	if s.guardrails == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	wsID, ok := s.effectiveWorkspaceID(r)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: no workspace identity"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.guardrails.OutputEnabled(),
		"policy":  s.guardrails.GetPolicy(wsID),
	})
}

// handleModalityCapabilities returns the model→capabilities map for the
// dashboard's capability view. Static, no state.
func (s *Server) handleModalityCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, modality.CapabilityMap())
}

// SetBudgetStore wires the budgets store used by the dashboard's Budgets
// panel. A setter so NewServer's signature stays put; a nil store makes
// handleBudgets return an empty list (panel stays hidden).
func (s *Server) SetBudgetStore(st *budgets.Store) { s.budgetStore = st }

// handleBudgets lists a workspace's budgets (defaulting to "default") for
// the ops dashboard. Always returns an array — never null — so the panel
// can hide itself when there are none.
func (s *Server) handleBudgets(w http.ResponseWriter, r *http.Request) {
	if s.budgetStore == nil {
		writeJSON(w, http.StatusOK, []budgets.Budget{})
		return
	}
	wsID, ok := s.effectiveWorkspaceID(r)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: no workspace identity"})
		return
	}
	list, err := s.budgetStore.List(r.Context(), wsID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []budgets.Budget{}
	}
	writeJSON(w, http.StatusOK, list)
}

// SetForecaster wires the cost forecaster used by the dashboard's
// projection columns. A setter so NewServer's signature stays put; a nil
// forecaster makes handleForecastSummary return an empty list.
func (s *Server) SetForecaster(f *forecast.Forecaster) { s.forecaster = f }

// handleForecastSummary returns a projection for every budget in the
// workspace. Always an array — never null — so the dashboard can hide the
// projections when there are none. Read-only + cached in the forecaster.
func (s *Server) handleForecastSummary(w http.ResponseWriter, r *http.Request) {
	if s.forecaster == nil {
		writeJSON(w, http.StatusOK, []forecast.Forecast{})
		return
	}
	wsID, ok := s.effectiveWorkspaceID(r)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: no workspace identity"})
		return
	}
	list, err := s.forecaster.SummarizeWorkspace(r.Context(), wsID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []forecast.Forecast{}
	}
	writeJSON(w, http.StatusOK, list)
}

// SetEvalPipeline wires the evaluation pipeline used by the dashboard's
// Evaluation panel. A setter so NewServer's signature stays put; a nil
// pipeline makes handleEvalRuns return an empty list (panel stays hidden).
func (s *Server) SetEvalPipeline(p *eval.Pipeline) { s.evalPipeline = p }

// handleEvalRuns returns the recent eval run summaries for a workspace so the
// dashboard can show pass/fail and highlight runs whose pass rate dipped. Off
// the hot path; read-only.
func (s *Server) handleEvalRuns(w http.ResponseWriter, r *http.Request) {
	if s.evalPipeline == nil {
		writeJSON(w, http.StatusOK, []eval.RunSummary{})
		return
	}
	wsID, ok := s.effectiveWorkspaceID(r)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: no workspace identity"})
		return
	}
	list, err := s.evalPipeline.ListRuns(r.Context(), wsID, 15)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []eval.RunSummary{}
	}
	writeJSON(w, http.StatusOK, list)
}

// SetPOVIStakeManager wires the PoVI node-staking manager for the dashboard's
// Staking panel. A nil manager makes handlePOVIStakes return an empty list
// (panel stays hidden).
func (s *Server) SetPOVIStakeManager(m *povi.StakeManager) { s.poviStakes = m }

// SetPOVIChallengeStore wires the challenge audit store for the dashboard's
// Challenges panel. A nil store makes handlePOVIChallenges return an empty list.
func (s *Server) SetPOVIChallengeStore(st *povi.ChallengeStore) { s.poviChallenges = st }

// handlePOVIChallenges returns recent challenges (pass/fail/timeout + slashes)
// for the Challenges panel. Off the hot path; read-only.
func (s *Server) handlePOVIChallenges(w http.ResponseWriter, r *http.Request) {
	if s.poviChallenges == nil {
		writeJSON(w, http.StatusOK, []povi.Challenge{})
		return
	}
	list, err := s.poviChallenges.List(r.Context(), r.URL.Query().Get("node"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []povi.Challenge{}
	}
	writeJSON(w, http.StatusOK, list)
}

// handlePOVIStakes returns the node stakes for the Staking panel (collateral,
// status, slashes). Off the hot path; read-only.
func (s *Server) handlePOVIStakes(w http.ResponseWriter, r *http.Request) {
	if s.poviStakes == nil {
		writeJSON(w, http.StatusOK, []povi.Stake{})
		return
	}
	list, err := s.poviStakes.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []povi.Stake{}
	}
	writeJSON(w, http.StatusOK, list)
}

// SetCostAnomalyDetector wires the cross-sectional cost-anomaly detector
// used by the dashboard's Cost outliers panel. A setter so NewServer's
// signature stays put; a nil detector makes handleCostAnomalies return an
// empty scan.
func (s *Server) SetCostAnomalyDetector(d *costanomaly.Detector) { s.costAnomaly = d }

// handleCostAnomalies returns the issue-scope anomaly scan for a workspace
// (the dashboard's default view). These are statistical flags, not
// judgments. Read-only + cached in the detector.
func (s *Server) handleCostAnomalies(w http.ResponseWriter, r *http.Request) {
	wsID, ok := s.effectiveWorkspaceID(r)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: no workspace identity"})
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = costanomaly.UnitIssue
	}
	if s.costAnomaly == nil {
		writeJSON(w, http.StatusOK, costanomaly.ScanResult{WorkspaceID: wsID, Scope: scope, Anomalies: []costanomaly.Anomaly{}})
		return
	}
	res, err := s.costAnomaly.ScanScope(r.Context(), wsID, scope)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// SetROIReporter wires the executive ROI reporter used by the dashboard's
// Executive summary panel. A setter so NewServer's signature stays put; a
// nil reporter makes handleROISummary return an empty summary.
func (s *Server) SetROIReporter(r *roi.Reporter) { s.roiReporter = r }

// handleROISummary returns the compact executive ROI summary for the
// dashboard. Read-only + cached in the reporter.
func (s *Server) handleROISummary(w http.ResponseWriter, r *http.Request) {
	wsID, ok := s.effectiveWorkspaceID(r)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: no workspace identity"})
		return
	}
	if s.roiReporter == nil {
		writeJSON(w, http.StatusOK, roi.ReportSummary{WorkspaceID: wsID, InsufficientData: true})
		return
	}
	summary, err := s.roiReporter.GenerateSummary(r.Context(), wsID, r.URL.Query().Get("period"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// SetRoutingAdvisor wires the pattern-network routing advisor used by the
// dashboard's Routing intelligence panel.
func (s *Server) SetRoutingAdvisor(a *routing.Advisor) { s.routingAdvisor = a }

// handleRoutingIntelligence returns the advisor status + per-cohort digest
// for the dashboard. Read-only, in-memory.
func (s *Server) handleRoutingIntelligence(w http.ResponseWriter, r *http.Request) {
	if s.routingAdvisor == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": routing.Status{}, "cohorts": []routing.CohortDigest{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  s.routingAdvisor.Status(),
		"cohorts": s.routingAdvisor.Overview(),
	})
}

// handleAnomalies runs Detect for the dimension tuple supplied via query
// params. Returns an empty array (not 204) when no anomalies fire so
// dashboards can render "no anomalies" without a null check.
func (s *Server) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	if s.anomalyDetector == nil {
		writeJSON(w, http.StatusOK, []anomaly.Anomaly{})
		return
	}
	q := r.URL.Query()
	wsID, ok := s.effectiveWorkspaceID(r)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: no workspace identity"})
		return
	}
	anoms, err := s.anomalyDetector.Detect(r.Context(), wsID, q.Get("team"), q.Get("feature"), q.Get("provider"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if anoms == nil {
		anoms = []anomaly.Anomaly{}
	}
	writeJSON(w, http.StatusOK, anoms)
}

// handleAnomaliesScan runs ScanAll across every active dimension. Used
// by the dashboard and by ops dashboards for tenant-wide views.
func (s *Server) handleAnomaliesScan(w http.ResponseWriter, r *http.Request) {
	if s.anomalyDetector == nil {
		writeJSON(w, http.StatusOK, []anomaly.Anomaly{})
		return
	}
	anoms, err := s.anomalyDetector.ScanAll(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if anoms == nil {
		anoms = []anomaly.Anomaly{}
	}
	writeJSON(w, http.StatusOK, anoms)
}

// -------------------------------------------------------------------------
// Spend analytics
// -------------------------------------------------------------------------

const spendSummarySQL = `SELECT COALESCE(SUM(cost_usd), 0),
  COALESCE(SUM(input_tokens), 0),
  COALESCE(SUM(output_tokens), 0),
  COUNT(*),
  COUNT(*) FILTER (WHERE cached)
FROM token_events
WHERE workspace_id = $1
  AND created_at > NOW() - INTERVAL '1 day' * $2`

func (s *Server) handleSpendSummary(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeError(w, http.StatusInternalServerError, "database not configured")
		return
	}
	wsID, ok := s.effectiveWorkspaceID(r)
	if !ok {
		writeError(w, http.StatusForbidden, "forbidden: no workspace identity")
		return
	}
	days := queryInt(r, "days", 30)

	var (
		totalCost                              float64
		totalIn, totalOut, totalReq, cachedReq int64
	)
	if err := s.pool.QueryRow(r.Context(), spendSummarySQL, wsID, days).
		Scan(&totalCost, &totalIn, &totalOut, &totalReq, &cachedReq); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hitRate := 0.0
	avgCost := 0.0
	if totalReq > 0 {
		hitRate = float64(cachedReq) / float64(totalReq)
		avgCost = totalCost / float64(totalReq)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_cost_usd":       totalCost,
		"total_input_tokens":   totalIn,
		"total_output_tokens":  totalOut,
		"total_requests":       totalReq,
		"cached_requests":      cachedReq,
		"cache_hit_rate":       hitRate,
		"avg_cost_per_request": avgCost,
		"period_days":          days,
	})
}

func (s *Server) handleSpendBy(dimension string) http.HandlerFunc {
	// Whitelist the column to prevent SQL injection.
	allowed := map[string]bool{"model": true, "team": true, "feature": true}
	if !allowed[dimension] {
		dimension = "model"
	}
	q := `SELECT ` + dimension + `,
  COALESCE(SUM(cost_usd), 0) AS cost_usd,
  COUNT(*) AS requests,
  COALESCE(SUM(input_tokens), 0) AS input_tokens,
  COALESCE(SUM(output_tokens), 0) AS output_tokens
FROM token_events
WHERE workspace_id = $1
  AND created_at > NOW() - INTERVAL '1 day' * $2
GROUP BY ` + dimension + `
ORDER BY cost_usd DESC`

	return func(w http.ResponseWriter, r *http.Request) {
		if s.pool == nil {
			writeError(w, http.StatusInternalServerError, "database not configured")
			return
		}
		wsID, ok := s.effectiveWorkspaceID(r)
		if !ok {
			writeError(w, http.StatusForbidden, "forbidden: no workspace identity")
			return
		}
		days := queryInt(r, "days", 30)

		rows, err := s.pool.Query(r.Context(), q, wsID, days)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()

		var out []map[string]any
		for rows.Next() {
			var (
				key                          string
				cost                         float64
				requests, inTokens, outTokens int64
			)
			if err := rows.Scan(&key, &cost, &requests, &inTokens, &outTokens); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			out = append(out, map[string]any{
				dimension:       key,
				"cost_usd":      cost,
				"requests":      requests,
				"input_tokens":  inTokens,
				"output_tokens": outTokens,
			})
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// -------------------------------------------------------------------------
// Cache analytics
// -------------------------------------------------------------------------

const cacheStatsSQL = `SELECT COUNT(*),
  COUNT(*) FILTER (WHERE cached),
  COALESCE(SUM(cost_usd) FILTER (WHERE NOT cached), 0)
FROM token_events
WHERE workspace_id = $1`

const cacheEntriesSQL = `SELECT COUNT(*) FROM prompt_embeddings`

func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeError(w, http.StatusInternalServerError, "database not configured")
		return
	}
	wsID, ok := s.effectiveWorkspaceID(r)
	if !ok {
		writeError(w, http.StatusForbidden, "forbidden: no workspace identity")
		return
	}

	var total, cached int64
	var uncachedCost float64
	if err := s.pool.QueryRow(r.Context(), cacheStatsSQL, wsID).Scan(&total, &cached, &uncachedCost); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var entries int64
	if err := s.pool.QueryRow(r.Context(), cacheEntriesSQL).Scan(&entries); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	totalRate := 0.0
	savings := 0.0
	if total > 0 {
		totalRate = float64(cached) / float64(total)
		savings = uncachedCost * (float64(cached) / float64(total))
	}
	// We don't track exact-vs-semantic split in token_events yet, so we
	// surface the same number on both fields. A future migration can add
	// the cache_layer column and split this honestly.
	writeJSON(w, http.StatusOK, map[string]any{
		"exact_hit_rate":        totalRate,
		"semantic_hit_rate":     totalRate,
		"total_hit_rate":        totalRate,
		"entries_count":         entries,
		"estimated_savings_usd": savings,
	})
}

const topPatternsSQL = `SELECT prompt_hash, hit_count, tokens_saved, updated_at
FROM prompt_embeddings
ORDER BY hit_count DESC
LIMIT $1`

func (s *Server) handleCacheTopPatterns(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeError(w, http.StatusInternalServerError, "database not configured")
		return
	}
	limit := queryInt(r, "limit", 10)
	if limit < 1 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	rows, err := s.pool.Query(r.Context(), topPatternsSQL, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var (
			hash        string
			hitCount    int64
			tokensSaved int64
			lastSeen    time.Time
		)
		if err := rows.Scan(&hash, &hitCount, &tokensSaved, &lastSeen); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, map[string]any{
			"prompt_hash":  hash,
			"hit_count":    hitCount,
			"tokens_saved": tokensSaved,
			"last_seen":    lastSeen,
		})
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// -------------------------------------------------------------------------
// Model recommendations
// -------------------------------------------------------------------------

// blendedPerTokenCost is a rough USD/token estimate used to translate
// "tokens saved" into "money saved". We pick gpt-4o-mini's blended rate
// because it's the most common cheap fallback in the cost table.
const blendedPerTokenCost = 0.000000375 // ≈ (0.15+0.60)/2 per million tokens

func (s *Server) handleModelsRecommendations(w http.ResponseWriter, r *http.Request) {
	if s.analyser == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	insights, err := s.analyser.Analyse(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(insights) > 10 {
		insights = insights[:10]
	}
	out := make([]map[string]any, 0, len(insights))
	for _, ins := range insights {
		est := float64(ins.HitCount) * float64(ins.AvgTokensSaved) * blendedPerTokenCost * 30
		out = append(out, map[string]any{
			"pattern_hash":                  ins.PromptPattern,
			"hit_count":                     ins.HitCount,
			"recommendation":                ins.Recommendation,
			"estimated_monthly_savings_usd": est,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// -------------------------------------------------------------------------
// Workspace summary
// -------------------------------------------------------------------------

const workspaceSpendSQL = `SELECT COALESCE(SUM(cost_usd), 0), COUNT(*)
FROM token_events
WHERE workspace_id = $1
  AND created_at > NOW() - INTERVAL '30 days'`

func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	// Authz (#146): the tenant roster (every workspace + per-workspace cost) is
	// an all-tenant view with no single-tenant shape, so it is ADMIN-ONLY. A
	// non-admin reads its own data via the path-scoped /v1/workspaces/{wsID}
	// routes; here it gets 403 rather than the full roster.
	if actx := auth.GetAuthContext(r.Context()); actx == nil || !actx.IsAdmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin credentials required"})
		return
	}
	if s.wsManager == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	all := s.wsManager.ListWorkspaces()
	out := make([]map[string]any, 0, len(all))
	for _, ws := range all {
		entry := map[string]any{
			"id":                     ws.ID,
			"name":                   ws.Name,
			"active":                 ws.Active,
			"logging_policy":         string(ws.LoggingPolicy),
			"current_month_cost_usd": 0.0,
			"request_count":          0,
		}
		if s.pool != nil {
			var cost float64
			var count int64
			if err := s.pool.QueryRow(r.Context(), workspaceSpendSQL, ws.ID).Scan(&cost, &count); err == nil {
				entry["current_month_cost_usd"] = cost
				entry["request_count"] = count
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// -------------------------------------------------------------------------
// Alerts
// -------------------------------------------------------------------------

func (s *Server) handleAlertsCircuits(w http.ResponseWriter, _ *http.Request) {
	if s.alertManager == nil {
		writeJSON(w, http.StatusOK, map[string]string{})
		return
	}
	writeJSON(w, http.StatusOK, s.alertManager.CircuitStates())
}

func (s *Server) handleAlertsRules(w http.ResponseWriter, _ *http.Request) {
	if s.alertManager == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.alertManager.Rules())
}

// -------------------------------------------------------------------------
// Local router status
// -------------------------------------------------------------------------

func (s *Server) handleLocalStatus(w http.ResponseWriter, _ *http.Request) {
	if s.localRouter == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available":        false,
			"models":           []any{},
			"requests_served":  0,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available":       s.localRouter.Available(),
		"models":          s.localRouter.Models(),
		"requests_served": 0, // tracked via Prometheus counter; not surfaced here yet
	})
}

// -------------------------------------------------------------------------
// Health
// -------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{
		"postgres": s.checkPostgres(r.Context()),
		"redis":    s.checkRedis(r.Context()),
		"nats":     s.checkNATS(),
	}
	status := "ok"
	for _, v := range checks {
		if v != "ok" {
			status = "degraded"
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         status,
		"version":        s.version,
		"uptime_seconds": int64(time.Since(s.startTime).Seconds()),
		"checks":         checks,
	})
}

func (s *Server) checkPostgres(ctx context.Context) string {
	if s.pool == nil {
		return "unconfigured"
	}
	if err := s.pool.Ping(ctx); err != nil {
		return "error"
	}
	return "ok"
}

func (s *Server) checkRedis(ctx context.Context) string {
	if s.redisClient == nil {
		return "unconfigured"
	}
	if err := s.redisClient.Ping(ctx).Err(); err != nil {
		return "error"
	}
	return "ok"
}

func (s *Server) checkNATS() string {
	if s.natsConn == nil {
		return "unconfigured"
	}
	if s.natsConn.Status() == nats.CONNECTED {
		return "ok"
	}
	return "error"
}

// -------------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func queryDefault(r *http.Request, key, fallback string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	return fallback
}

// effectiveWorkspaceID is the /v1/api/* dashboard chokepoint for the #146 Phase-2
// cross-tenant read fix: it resolves which workspace a read may scope to from the
// AUTHENTICATED credential, never the caller-supplied ?workspace_id=. A NON-ADMIN
// is forced to its OWN workspace — the query param is ignored AND the legacy
// "default" fallback never applies, so a tenant can read neither another tenant's
// data nor the shared "default" tenant's. The global ADMIN honors ?workspace_id=
// (empty → the historical "default", preserving the ops dashboard). ok is false
// only when a non-admin has no resolvable workspace; the handler 403s.
func (s *Server) effectiveWorkspaceID(r *http.Request) (string, bool) {
	ws, isAdmin := auth.WorkspaceIdentity(r.Context())
	if isAdmin {
		return queryDefault(r, "workspace_id", "default"), true
	}
	if ws == "" {
		return "", false
	}
	return ws, true
}

func queryInt(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// ensure errors import is used; future endpoints may surface wrapped errors.
var _ = errors.New
