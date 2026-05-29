package main

import (
	"context"
	"fmt"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/talyvor/lens/internal/ab"
	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/anomaly"
	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/batch"
	"github.com/talyvor/lens/internal/budget"
	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/costanomaly"
	"github.com/talyvor/lens/internal/forecast"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/compat"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/dashboard"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/eval"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/keypool"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/mcp"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/oracle"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/prompts"
	"github.com/talyvor/lens/internal/proxy"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/internal/ratelimit"
	"github.com/talyvor/lens/internal/retry"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/status"
	"github.com/talyvor/lens/internal/templates"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/warmer"
	"github.com/talyvor/lens/internal/workspace"
)

func main() {
	if err := run(); err != nil {
		slog.Error("startup failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tp, err := initTracing(ctx)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			logger.Warn("tracer shutdown", slog.String("err", err.Error()))
		}
	}()

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return err
	}
	redisClient := redis.NewClient(redisOpts)
	defer func() { _ = redisClient.Close() }()

	if err := redisClient.Ping(ctx).Err(); err != nil {
		logger.Warn("redis ping failed", slog.String("err", err.Error()))
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		logger.Warn("postgres ping failed", slog.String("err", err.Error()))
	}

	nc, err := nats.Connect(cfg.NatsURL)
	if err != nil {
		return err
	}
	defer nc.Close()

	exactCache := cache.NewExactCache(redisClient, cfg.MaxCacheTTL)
	openAIEmbedder := embedder.NewOpenAIEmbedder(cfg.OpenAIAPIKey, cfg.EmbeddingModel)
	semanticCache := cache.NewSemanticCache(pool, openAIEmbedder, cfg.SemanticThreshold, cfg.MaxCacheTTL)
	promptCompressor := compressor.New()
	modelRouter := router.New()
	piiDetector := pii.New()
	alertManager := alerts.New(pool, nc, nil) // rules loaded from DB in a future iteration
	alertManager.StartMonitor(ctx)
	templateDetector := templates.New(pool)
	qualityScorer := quality.New(pool)
	abTester := ab.New(pool, qualityScorer)
	abEngine := ab.NewEngine(pool)
	abEngine.RunAutoCompleteLoop(ctx, time.Hour)
	branchTracker := attribution.New(pool)
	attrStore := attribution.NewStore(pool)
	budgetStore := budgets.NewStore(pool)
	budgetService := budgets.NewService(budgetStore)
	// Predictive cost forecasting (Upgrade 20). Read-only analytics over
	// token_events + budgets; cached, off the request hot path.
	forecaster := forecast.New(forecast.NewStore(pool))
	// Cross-sectional cost anomaly detection (Upgrade 21). Read-only,
	// cached, off the hot path. Distinct from the temporal anomaly.Detector.
	costAnomalyDetector := costanomaly.New(costanomaly.NewStore(pool))
	wsManager := workspace.New(pool)
	if err := wsManager.LoadAll(ctx); err != nil {
		logger.Warn("workspace: LoadAll failed", slog.String("err", err.Error()))
	}
	if err := wsManager.RegisterWorkspace(ctx, workspace.Workspace{
		ID: "default", Name: "Default Workspace", Active: true,
	}); err != nil {
		logger.Warn("workspace: default registration failed", slog.String("err", err.Error()))
	}

	lr := localrouter.New(cfg.OllamaURL)
	go lr.StartHealthCheck(ctx)

	// Multi-endpoint local router (additive, see internal/localrouter/multi.go).
	// Parses LENS_LOCAL_ENDPOINTS; if empty, the registry stays empty and
	// the admin API can register endpoints dynamically at runtime.
	localRouterMulti := localrouter.NewRouterFromConfig(pool, cfg.LocalEndpoints)
	localRouterMulti.StartHealthChecks(ctx, localrouter.DefaultHealthCheckInterval)
	injectionDetector := injection.New(injection.DefaultPolicy())
	budgetEnforcer := budget.New(pool, budget.BudgetPolicy{MaxOutputTokens: 4096})
	batchRouter := batch.New(pool, cfg.AnthropicAPIKey)
	go batchRouter.StartPoller(ctx)
	sessionTracker := session.New(pool)
	go sessionTracker.StartCleanup(ctx)

	promptManager := prompts.New(pool)
	fallbackRouter := fallback.New()
	keyPool := keypool.New()
	auditExporter := audit.New(pool)
	evalPipeline := eval.New(pool, qualityScorer, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey, cfg.GoogleAPIKey)
	anomalyDetector := anomaly.New(pool)
	go anomalyDetector.StartMonitor(ctx, nc, 1*time.Hour)
	statusPage := status.New(pool, redisClient, nc, "0.1.0")
	go statusPage.StartCacher(ctx, 60*time.Second)
	guardrailsEngine := guardrails.New(piiDetector, injectionDetector)

	l := learner.New(nc, pool)
	go l.StartBackground(ctx)

	cacheWarmer := warmer.New(pool, l, exactCache, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey)
	go cacheWarmer.Start(ctx, 1*time.Hour)

	p := proxy.New(exactCache, semanticCache, openAIEmbedder, promptCompressor, modelRouter, piiDetector, alertManager, templateDetector, qualityScorer, abTester, branchTracker, wsManager, lr, injectionDetector, budgetEnforcer, batchRouter, sessionTracker, promptManager, fallbackRouter, keyPool, auditExporter, guardrailsEngine, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey, cfg.GoogleAPIKey, l)
	// Upgraded per-request attribution store (Upgrade Batch 1 / Item 3).
	// Wired as a setter so the existing proxy.New signature stays put.
	p.SetAttributionStore(attrStore)
	// Per-team / per-sprint budget governance (Upgrade 19). Seed the
	// in-memory snapshot from token_events, refresh it periodically, then
	// wire the gate into the proxy hot path. Load is best-effort — a cold
	// start simply begins with zero budgets until the first refresh.
	if err := budgetService.Load(ctx); err != nil {
		logger.Warn("budgets: initial load failed", slog.String("err", err.Error()))
	}
	budgetService.StartRefresh(ctx)
	p.SetBudgetService(budgetService)
	p.SetBedrockConfig(proxy.BedrockConfig{
		Region:          cfg.AWSRegion,
		AccessKeyID:     cfg.AWSAccessKeyID,
		SecretAccessKey: cfg.AWSSecretAccessKey,
		SessionToken:    cfg.AWSSessionToken,
	})
	p.SetExtraProviderConfig(proxy.ExtraProviderConfig{
		MistralKey: cfg.MistralAPIKey,
		GroqKey:    cfg.GroqAPIKey,
		VLLMURL:    cfg.VLLMBaseURL,
		VLLMKey:    cfg.VLLMAPIKey,
	})

	keyStore := auth.New(pool)
	if err := keyStore.LoadAll(ctx); err != nil {
		logger.Warn("auth: LoadAll failed", slog.String("err", err.Error()))
	}
	tenantStore := tenant.NewStore(pool)
	spendTracker := tenant.NewSpendTracker(tenantStore)
	rateLimiter := ratelimit.New(redisClient, ratelimit.DefaultRules())

	// Token-bucket multi-tier limiter (Item 8). Global tier is
	// configured from env; per-workspace tiers are layered on at
	// request time by callers that build a per-request limiter
	// from tenant.WorkspaceConfig. Coexists with the legacy
	// sliding-window limiter above.
	multiTierLimiter := ratelimit.NewMultiTierLimiter(redisClient,
		ratelimit.LimitTier{
			Name: "global",
			Key:  "global",
			RPM:  cfg.GlobalRPM,
			TPM:  cfg.GlobalTPM,
		},
	)
	_ = multiTierLimiter // exposed for future per-request wiring

	// Auth manager — unified JWT + workspace-key + global-key
	// authenticator. Coexists with the legacy auth.AuthMiddleware
	// (which is still mounted below for backward compat); new
	// /v1/auth/* routes use authManager directly.
	authManager := auth.NewManager(os.Getenv("LENS_API_KEY"), cfg.JWTSecret, keyStore, tenantStore)

	// Retry policy + per-provider circuit breaker registry (Item 9).
	// Coexists with the legacy retry.Do helper.
	retryPolicy := retry.DefaultPolicy
	if cfg.RetryMaxAttempts > 0 {
		retryPolicy.MaxAttempts = cfg.RetryMaxAttempts
	}
	if cfg.RetryInitialDelay > 0 {
		retryPolicy.InitialDelay = cfg.RetryInitialDelay
	}
	if cfg.RetryMaxDelay > 0 {
		retryPolicy.MaxDelay = cfg.RetryMaxDelay
	}
	breakerRegistry := retry.NewBreakerRegistry(cfg.CBThreshold, cfg.CBResetTimeout, 2)
	// The circuit state-change hook (structured logging + HA gossip publish) is
	// installed by setupHA below, so the two compose in one place and behave
	// identically whether or not HA is enabled.
	retryExecutor := retry.NewExecutor(&retryPolicy, breakerRegistry)
	_ = retryExecutor // wired into proxy paths in a follow-up

	// High-Availability layer (Upgrade 7). Strictly opt-in via LENS_HA_ENABLED;
	// a no-op / in-process fallback when disabled, so single-instance behaviour
	// is unchanged. Built here so its health handlers are mountable below and
	// its registry/breaker drive graceful drain on shutdown.
	haComps := setupHA(ctx, cfg, redisClient, pool, breakerRegistry, rateLimiter, logger)

	// LENS token mining ledger + cache-mining engine (Batch 2 Item 1).
	tokenLedger := mining.NewLedgerStore(pool)
	cacheMiner := mining.NewCacheMiner(tokenLedger, cfg.CacheSharingEnabled)
	_ = cacheMiner // hooks into the cache-hit path in a follow-up wire-up

	// Compute mining (Batch 2 Item 2). Wires its hook into the
	// localrouter so any verified GPU node that serves a
	// cross-workspace request gets credited automatically.
	computeMiner := mining.NewComputeMiner(tokenLedger, pool)
	localRouterMulti.SetOnRequestServed(func(nodeID, requesterWS string, tokens int, latencyMs int64) {
		if err := computeMiner.RecordServedRequest(ctx, nodeID, requesterWS, tokens, latencyMs); err != nil {
			logger.Warn("compute mining: record served request failed",
				slog.String("node_id", nodeID), slog.String("err", err.Error()))
		}
	})

	// Embedding mining (Batch 2 Item 3). Shares the ledger;
	// proxy wires RecordEmbeddingsServed in the semantic-cache
	// path in a follow-up.
	embeddingMiner := mining.NewEmbeddingMiner(tokenLedger, pool)
	_ = embeddingMiner

	// Annotation mining (Batch 2 Item 4). Proof-of-useful-work
	// — annotators stake LENS, review pairs of responses, and
	// earn per validated annotation.
	annotationMiner := mining.NewAnnotationMiner(tokenLedger, pool)

	// Pattern mining (Batch 2 Item 5). The deployment-level
	// PatternMiningEnabled flag ANDs with the per-workspace
	// opt-in before earnings fire (RecordPattern's optedIn arg).
	patternMiner := mining.NewPatternMiner(tokenLedger, pool)

	// Token marketplace + staking (Batch 3 Phase 1).
	marketplace := economy.NewMarketplaceStore(tokenLedger, pool)

	// Quality Oracle (Batch 3 Phase 5). Wraps the existing
	// annotation miner with a 1% request sampler + the
	// dashboard rollup query.
	oracleEngine := oracle.New(annotationMiner, tokenLedger, pool)

	// Two-token split (Master Plan Upgrade 1). RateEngine derives
	// the LENS->LXC conversion rate from backing + supply; the
	// admin can only approve its output. DualTokenStore owns the
	// one-way LENS->LXC conversion + LXC spend path.
	rateEngine := economy.NewRateEngine(tokenLedger, pool)
	dualToken := economy.NewDualTokenStore(tokenLedger, pool, rateEngine)

	r := chi.NewRouter()
	// OTel HTTP middleware runs FIRST so every route — authenticated or
	// not — is traced and any incoming W3C traceparent header is extracted
	// into the request context before downstream middleware sees it.
	r.Use(otelhttp.NewMiddleware("talyvor-lens",
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithPropagators(otel.GetTextMapPropagator()),
	))
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	// Production-API middlewares — must run *after* the chi RequestID
	// middleware so our X-Request-ID handler can honour an incoming
	// header without colliding with chi's internal request-id field.
	r.Use((&api.RequestIDMiddleware{}).Handler)
	r.Use(api.APIVersionMiddleware)
	r.Use(api.GzipMiddleware)
	r.Use(api.RateLimitHeadersMiddleware)
	// HTTP request metrics (Upgrade 11): request count, latency histogram, and
	// in-flight gauge — labelled by chi route pattern (bounded cardinality).
	r.Use(metrics.HTTPMiddleware)

	// Detailed /healthz pings DB + Redis + the multi-endpoint local
	// router. Each checker has a 100ms budget so the rollup stays fast.
	healthHandler := api.NewHealthHandler("0.1.0", map[string]api.HealthChecker{
		"database": api.HealthCheckFunc(func(ctx context.Context) (bool, int64, string) {
			if pool == nil {
				return false, 0, "no pool configured"
			}
			start := time.Now()
			if err := pool.Ping(ctx); err != nil {
				return false, time.Since(start).Milliseconds(), err.Error()
			}
			return true, time.Since(start).Milliseconds(), ""
		}),
		"redis": api.HealthCheckFunc(func(ctx context.Context) (bool, int64, string) {
			if redisClient == nil {
				return false, 0, "no redis client"
			}
			start := time.Now()
			if err := redisClient.Ping(ctx).Err(); err != nil {
				return false, time.Since(start).Milliseconds(), err.Error()
			}
			return true, time.Since(start).Milliseconds(), ""
		}),
		"local_models": api.HealthCheckFunc(func(_ context.Context) (bool, int64, string) {
			eps := localRouterMulti.List()
			if len(eps) == 0 {
				return true, 0, ""
			}
			healthy := 0
			for _, e := range eps {
				if e.Healthy {
					healthy++
				}
			}
			if healthy == 0 {
				return false, 0, "0/" + strconv.Itoa(len(eps)) + " endpoints healthy"
			}
			if healthy < len(eps) {
				return true, 0, strconv.Itoa(len(eps)-healthy) + "/" + strconv.Itoa(len(eps)) + " endpoints unhealthy"
			}
			return true, 0, ""
		}),
	})
	r.Get("/healthz", healthHandler.ServeHTTP)

	// HA endpoints (Upgrade 7). The existing /healthz above is intentionally
	// left unchanged for backward-compat; these are additive:
	//   /livez     — pure liveness, always 200 while the process serves
	//   /readyz    — drain-aware readiness; 503 while draining or a dep is down
	//   /ha/status — cluster view (this instance + peers) for ops + dashboard
	r.Get("/livez", haComps.health.Live)
	r.Get("/readyz", haComps.health.Ready)
	r.Get("/ha/status", haComps.health.Status)

	// OpenAPI spec — public, no auth, no version path prefix.
	r.Get("/openapi.json", api.ServeOpenAPI)

	// Public token-rate endpoint — no auth needed; rates are
	// part of Lens's public economic surface.
	r.Get("/v1/tokens/rates", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cache": mining.Rates(),
			"compute": map[string]float64{
				"base_per_1k_tokens": mining.ComputeMineBaseRate,
				"multiplier_cpu":     mining.GPUMultiplierCPU,
				"multiplier_rtx4090": mining.GPUMultiplierRTX4090,
				"multiplier_a100":    mining.GPUMultiplierA100,
				"multiplier_h100":    mining.GPUMultiplierH100,
			},
			"embedding":  mining.EmbeddingRates(),
			"annotation": mining.AnnotationRates(),
			"pattern":    mining.PatternRates(),
		})
	})

	// Public economy stats (Batch 3 Phase 1).
	r.Get("/v1/economy/stats", func(w http.ResponseWriter, req *http.Request) {
		stats, err := marketplace.GetEconomyStats(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, stats)
	})

	// Public marketplace listings (read-only — no auth needed
	// to browse; buying requires auth).
	r.Get("/v1/marketplace/listings", func(w http.ResponseWriter, req *http.Request) {
		limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
		listings, err := marketplace.GetListings(req.Context(), limit)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, listings)
	})

	// Public insights endpoint — aggregated patterns across all
	// opted-in workspaces. Filters supported via query params.
	r.Get("/v1/insights/routing", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		insights, err := patternMiner.GetInsights(req.Context(),
			q.Get("model"), q.Get("provider"), q.Get("feature"))
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, insights)
	})

	// Public discovery: embedding nodes available for a model.
	r.Get("/v1/embedding-nodes/available", func(w http.ResponseWriter, req *http.Request) {
		model := req.URL.Query().Get("model")
		if model == "" {
			writeJSONErr(w, http.StatusBadRequest, "model query param required")
			return
		}
		minDim, _ := strconv.Atoi(req.URL.Query().Get("min_dimensions"))
		nodes, err := embeddingMiner.ListAvailableNodes(req.Context(), model, minDim)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, nodes)
	})

	// Public discovery: GPU nodes available for a model.
	r.Get("/v1/nodes/available", func(w http.ResponseWriter, req *http.Request) {
		model := req.URL.Query().Get("model")
		if model == "" {
			writeJSONErr(w, http.StatusBadRequest, "model query param required")
			return
		}
		nodes, err := computeMiner.ListAvailableNodes(req.Context(), model)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, nodes)
	})

	r.Handle("/metrics", metrics.Handler())

	apiServer := api.NewServer(
		pool, redisClient, nc, exactCache, l,
		alertManager, abTester, branchTracker, wsManager, lr,
		anomalyDetector,
		"0.1.0",
	)
	// Budgets store powers the dashboard's read-only Budgets panel.
	apiServer.SetBudgetStore(budgetStore)
	// Forecaster powers the dashboard's projection columns.
	apiServer.SetForecaster(forecaster)
	// Cost-anomaly detector powers the dashboard's Cost outliers panel.
	apiServer.SetCostAnomalyDetector(costAnomalyDetector)
	// Public: health probe and Prometheus passthrough never require a key.
	apiServer.MountUnauthenticated(r)

	// MCP server is exposed without API-key auth — MCP clients (Claude
	// Desktop, agent frameworks, etc.) bring their own auth model.
	mcpServer := mcp.New(pool, l, alertManager, wsManager, sessionTracker, "0.1.0")
	r.Post("/mcp", mcpServer.HandleRPC)
	r.Get("/mcp/sse", mcpServer.HandleSSE)

	// Dashboard is public — same trust model as /healthz. The dashboard
	// page itself is static; the live numbers come from /v1/api/* XHRs
	// that the browser sends with the user's own API key.
	dashHandler := dashboard.New("0.1.0")
	r.Get("/dashboard", dashHandler.ServeHTTP)
	r.Get("/dashboard/tokens", dashHandler.ServeTokens)
	r.Get("/dashboard/nodes", dashHandler.ServeNodes)
	r.Get("/dashboard/oracle", dashHandler.ServeOracle)
	r.Get("/dashboard/economy", dashHandler.ServeEconomy)
	r.Get("/", dashHandler.RedirectRoot)

	// Public oracle stats — no auth, no PII, just rollup counters.
	r.Get("/v1/oracle/stats", func(w http.ResponseWriter, req *http.Request) {
		stats, err := oracleEngine.GetOracleStats(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, stats)
	})

	// Public conversion-rate surface (two-token split). The rate +
	// its full derivation are public economic signal; no auth.
	r.Get("/v1/economy/conversion-rate", func(w http.ResponseWriter, req *http.Request) {
		rate, err := rateEngine.CurrentRate(req.Context())
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{
			"rate":        rate,
			"usd_per_lxc": economy.LXCUSDValue,
			"lens_per_lxc": rate,
		})
	})

	r.Get("/v1/economy/conversion-rate/history", func(w http.ResponseWriter, req *http.Request) {
		limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
		hist, err := rateEngine.RateHistory(req.Context(), limit)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, hist)
	})

	// Public status page. /status content-negotiates between HTML and
	// JSON; /status.json is the unconditional-JSON convenience route
	// uptime monitors and CI scripts use.
	r.Get("/status", statusPage.ServeHTTP)
	r.Get("/status.json", statusPage.ServeJSON)

	// Everything else sits behind the API-key middleware. chi.Group inherits
	// middleware only for routes registered inside its closure.
	heliconeCompat := compat.NewHeliconeCompat(keyStore)

	r.Group(func(authed chi.Router) {
		// Helicone-compat translates Helicone-* headers and rewrites
		// /oai/* and /anthropic/* paths BEFORE auth runs. That order
		// matters: Helicone-Auth becomes Authorization, which is what
		// AuthMiddleware then validates.
		authed.Use(heliconeCompat.Middleware())
		// Auth must run before the rate-limiter so the limiter sees the
		// key/workspace that AuthMiddleware just stamped onto the request.
		authed.Use(auth.AuthMiddleware(keyStore))
		authed.Use(ratelimit.RateLimitMiddleware(rateLimiter))

		apiServer.MountAuthenticated(authed)

		authed.Post("/v1/proxy/openai/*", p.HandleOpenAI)
		authed.Post("/v1/proxy/anthropic/*", p.HandleAnthropic)
		authed.Post("/v1/proxy/google/*", p.HandleGoogle)
		authed.Post("/v1/proxy/bedrock/*", p.HandleBedrock)
		authed.Post("/v1/proxy/mistral/*", p.HandleExtraProvider("mistral"))
		authed.Post("/v1/proxy/groq/*", p.HandleExtraProvider("groq"))
		authed.Post("/v1/proxy/vllm/*", p.HandleExtraProvider("vllm"))

		// Helicone-shape URL prefixes. First-class routes (not a
		// deprecated alias) — migrating teams can keep these URLs
		// indefinitely. The compat middleware above strips the
		// Helicone-Auth / Helicone-Property-* headers before the
		// proxy handler runs.
		authed.Post("/oai/*", p.HandleOpenAI)
		authed.Post("/anthropic/*", p.HandleAnthropic)

		authed.Post("/v1/api/keys", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				WorkspaceID string     `json:"workspace_id"`
				Team        string     `json:"team"`
				Name        string     `json:"name"`
				ExpiresAt   *time.Time `json:"expires_at,omitempty"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON: " + err.Error()})
				return
			}
			if in.WorkspaceID == "" {
				in.WorkspaceID = "default"
			}
			raw, apiKey, err := keyStore.GenerateKey(req.Context(), in.WorkspaceID, in.Team, in.Name, in.ExpiresAt)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"key":     raw,
				"id":      apiKey.ID,
				"warning": "Store this key securely. It will not be shown again.",
			})
		})

		authed.Post("/v1/api/injection/patterns", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				Pattern string `json:"pattern"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := injectionDetector.AddPattern(in.Pattern); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]bool{"ok": true})
		})

		authed.Delete("/v1/api/keys/{keyID}", func(w http.ResponseWriter, req *http.Request) {
			keyID := chi.URLParam(req, "keyID")
			if err := keyStore.Revoke(req.Context(), keyID); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		})

		// ─── JWT auth endpoints ────────────────────────
		// /v1/auth/token mints a JWT — admin-only because issuing
		// a token for an arbitrary workspace/user is a privileged
		// op. /refresh accepts any valid JWT and extends it.
		// /me echoes the resolved AuthContext for the caller.
		authed.Post("/v1/auth/token", func(w http.ResponseWriter, req *http.Request) {
			if authManager.JWTSecret() == "" {
				writeJSONErr(w, http.StatusServiceUnavailable, "JWT signing disabled (no LENS_JWT_SECRET configured)")
				return
			}
			// Require global-admin auth via the unified manager.
			actx, err := authManager.Authenticate(req)
			if err != nil || !actx.IsAdmin {
				writeJSONErr(w, http.StatusForbidden, "admin credentials required")
				return
			}
			var in struct {
				WorkspaceID string   `json:"workspace_id"`
				UserID      string   `json:"user_id"`
				Scopes      []string `json:"scopes"`
				TTLHours    int      `json:"ttl_hours"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.WorkspaceID == "" {
				writeJSONErr(w, http.StatusBadRequest, "workspace_id required")
				return
			}
			ttl := auth.ClampTTL(time.Duration(in.TTLHours) * time.Hour)
			tok, err := auth.GenerateToken(in.WorkspaceID, in.UserID, in.Scopes, authManager.JWTSecret(), ttl)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]any{
				"token":      tok,
				"expires_at": time.Now().Add(ttl).UTC().Format(time.RFC3339),
			})
		})

		// Admin-only: approve the LENS->LXC conversion rate. The
		// admin can ONLY approve the algorithm's output (within
		// band + floor) — there is no way to set an arbitrary rate.
		// The full computation (all inputs) is returned + persisted.
		authed.Post("/v1/admin/conversion-rate/approve", func(w http.ResponseWriter, req *http.Request) {
			actx, err := authManager.Authenticate(req)
			if err != nil || !actx.IsAdmin {
				writeJSONErr(w, http.StatusForbidden, "admin credentials required")
				return
			}
			approvedBy := actx.UserID
			if approvedBy == "" {
				approvedBy = "global_key"
			}
			comp, err := rateEngine.ApproveRate(req.Context(), approvedBy)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, comp)
		})

		authed.Post("/v1/auth/refresh", func(w http.ResponseWriter, req *http.Request) {
			if authManager.JWTSecret() == "" {
				writeJSONErr(w, http.StatusServiceUnavailable, "JWT signing disabled")
				return
			}
			actx, err := authManager.Authenticate(req)
			if err != nil || actx.AuthMethod != auth.MethodJWT {
				writeJSONErr(w, http.StatusUnauthorized, "valid JWT required")
				return
			}
			ttl := auth.ClampTTL(cfg.TokenTTL)
			tok, err := auth.GenerateToken(actx.WorkspaceID, actx.UserID, actx.Scopes, authManager.JWTSecret(), ttl)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{
				"token":      tok,
				"expires_at": time.Now().Add(ttl).UTC().Format(time.RFC3339),
			})
		})

		authed.Get("/v1/auth/me", func(w http.ResponseWriter, req *http.Request) {
			actx, err := authManager.Authenticate(req)
			if err != nil {
				writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			writeJSONOK(w, http.StatusOK, actx)
		})

		// ─── Key rotation + usage ──────────────────────
		authed.Post("/v1/workspaces/{wsID}/api-keys/{keyID}/rotate", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			keyID := chi.URLParam(req, "keyID")
			// Look up the old key to preserve its scopes + name.
			keys, err := tenantStore.ListAPIKeys(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			var old *tenant.WorkspaceAPIKey
			for i := range keys {
				if keys[i].ID == keyID {
					old = &keys[i]
					break
				}
			}
			if old == nil {
				writeJSONErr(w, http.StatusNotFound, "key not found")
				return
			}
			raw, fresh, err := tenantStore.CreateAPIKey(req.Context(), wsID, old.Name, old.Scopes, old.ExpiresAt)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			// Best-effort: delete the old key.
			if err := tenantStore.RevokeAPIKey(req.Context(), old.ID); err != nil {
				logger.Warn("auth: rotate revoke old failed",
					slog.String("key_id", old.ID),
					slog.String("err", err.Error()))
			}
			logger.Info("auth: key rotated",
				slog.String("workspace_id", wsID),
				slog.String("old_key_id", old.ID),
				slog.String("new_key_id", fresh.ID),
			)
			writeJSONOK(w, http.StatusCreated, map[string]any{
				"key":     raw,
				"id":      fresh.ID,
				"prefix":  fresh.KeyPrefix,
				"warning": "Old key revoked. Store this new key securely — it will not be shown again.",
			})
		})

		authed.Get("/v1/workspaces/{wsID}/api-keys/{keyID}/usage", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			keyID := chi.URLParam(req, "keyID")
			keys, err := tenantStore.ListAPIKeys(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			for _, k := range keys {
				if k.ID == keyID {
					writeJSONOK(w, http.StatusOK, map[string]any{
						"last_used_at":   k.LastUsedAt,
						"total_requests": 0, // wired to token_events in a later upgrade
						"total_cost":     0,
					})
					return
				}
			}
			writeJSONErr(w, http.StatusNotFound, "key not found")
		})

		authed.Post("/v1/workspaces", func(w http.ResponseWriter, req *http.Request) {
			var in workspace.Workspace
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.RegisterWorkspace(req.Context(), in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]string{"id": in.ID})
		})

		authed.Get("/v1/workspaces/{wsID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			ws, ok := wsManager.GetWorkspace(wsID)
			if !ok {
				writeJSONErr(w, http.StatusNotFound, "workspace not found")
				return
			}
			writeJSONOK(w, http.StatusOK, ws)
		})

		// ─── Workspace tenant config + keys ─────────────────
		// Per-workspace policy bundle (spend cap, model + provider
		// allowlists, retention) and workspace-scoped API keys.
		// These sit alongside the Lens-admin key routes above —
		// admin keys authenticate the call, then operate on
		// workspace-scoped credentials.
		authed.Get("/v1/workspaces/{wsID}/config", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			cfg, err := tenantStore.GetConfig(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if cfg == nil {
				writeJSONErr(w, http.StatusNotFound, "no workspace config")
				return
			}
			writeJSONOK(w, http.StatusOK, cfg)
		})

		authed.Put("/v1/workspaces/{wsID}/config", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in tenant.WorkspaceConfig
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in.ID = wsID
			if err := tenantStore.UpsertConfig(req.Context(), in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			// Drop any cached spend snapshot so the new cap kicks
			// in on the next request instead of waiting for the TTL.
			spendTracker.InvalidateCache(wsID)
			writeJSONOK(w, http.StatusOK, in)
		})

		authed.Post("/v1/workspaces/{wsID}/api-keys", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				Name      string     `json:"name"`
				Scopes    []string   `json:"scopes"`
				ExpiresAt *time.Time `json:"expires_at,omitempty"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			raw, key, err := tenantStore.CreateAPIKey(req.Context(), wsID, in.Name, in.Scopes, in.ExpiresAt)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]any{
				"key":     raw,
				"id":      key.ID,
				"prefix":  key.KeyPrefix,
				"name":    key.Name,
				"scopes":  key.Scopes,
				"warning": "Store this key securely. It will not be shown again.",
			})
		})

		authed.Get("/v1/workspaces/{wsID}/api-keys", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			keys, err := tenantStore.ListAPIKeys(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, keys)
		})

		authed.Delete("/v1/workspaces/{wsID}/api-keys/{keyID}", func(w http.ResponseWriter, req *http.Request) {
			keyID := chi.URLParam(req, "keyID")
			if err := tenantStore.RevokeAPIKey(req.Context(), keyID); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// ─── per-team / per-sprint budgets (Upgrade 19) ───
		// CRUD + a live status endpoint. Every mutation reloads the budget
		// service's in-memory snapshot so edits take effect on the next
		// request without waiting for the periodic refresh.

		authed.Post("/v1/workspaces/{wsID}/budgets", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in budgets.Budget
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in.WorkspaceID = wsID
			created, err := budgetStore.Create(req.Context(), in)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			_ = budgetService.Reload(req.Context())
			writeJSONOK(w, http.StatusCreated, created)
		})

		authed.Get("/v1/workspaces/{wsID}/budgets", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			list, err := budgetStore.List(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		// status MUST be registered before the {id} route so the literal
		// segment wins; chi matches static paths ahead of wildcards anyway,
		// but keeping them adjacent documents the intent.
		authed.Get("/v1/workspaces/{wsID}/budgets/status", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			team := req.URL.Query().Get("team")
			sprint := req.URL.Query().Get("sprint")
			writeJSONOK(w, http.StatusOK, budgetService.Status(wsID, team, sprint))
		})

		authed.Get("/v1/workspaces/{wsID}/budgets/{id}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			id := chi.URLParam(req, "id")
			b, err := budgetStore.Get(req.Context(), wsID, id)
			if errors.Is(err, budgets.ErrNotFound) {
				writeJSONErr(w, http.StatusNotFound, "budget not found")
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, b)
		})

		authed.Patch("/v1/workspaces/{wsID}/budgets/{id}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			id := chi.URLParam(req, "id")
			var in budgets.Budget
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			updated, err := budgetStore.Update(req.Context(), wsID, id, in)
			if errors.Is(err, budgets.ErrNotFound) {
				writeJSONErr(w, http.StatusNotFound, "budget not found")
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			_ = budgetService.Reload(req.Context())
			writeJSONOK(w, http.StatusOK, updated)
		})

		authed.Delete("/v1/workspaces/{wsID}/budgets/{id}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			id := chi.URLParam(req, "id")
			if err := budgetStore.Delete(req.Context(), wsID, id); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			_ = budgetService.Reload(req.Context())
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// ─── cost forecasting (Upgrade 20) ───
		// Read-only, cached projections. scope defaults to workspace,
		// scope_id to the workspace id, period to monthly.
		authed.Get("/v1/workspaces/{wsID}/forecast", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			q := req.URL.Query()
			scope := budgets.Scope(q.Get("scope"))
			if scope == "" {
				scope = budgets.ScopeWorkspace
			}
			fc, err := forecaster.ProjectScope(req.Context(), wsID, scope, q.Get("scope_id"), q.Get("period"))
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, fc)
		})

		authed.Get("/v1/workspaces/{wsID}/forecast/summary", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			list, err := forecaster.SummarizeWorkspace(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		// ─── cost anomaly detection (Upgrade 21) ───
		// Read-only, cached cross-sectional outlier flags. scope is the unit
		// kind (issue/team/sprint); default issue. These are statistical
		// flags, not judgments.
		authed.Get("/v1/workspaces/{wsID}/anomalies", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			scope := req.URL.Query().Get("scope")
			if scope == "" {
				scope = costanomaly.UnitIssue
			}
			res, err := costAnomalyDetector.ScanScope(req.Context(), wsID, scope)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, res)
		})

		authed.Get("/v1/workspaces/{wsID}/anomalies/issue/{issueID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			issueID := chi.URLParam(req, "issueID")
			a, err := costAnomalyDetector.CheckIssue(req.Context(), wsID, issueID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, a)
		})

		// ─── Local endpoint registry ───────────────────
		// Admin surface for the multi-endpoint Router. Listing
		// is read-only; the other three routes mutate the
		// in-process registry (no persistence — restarts re-read
		// LENS_LOCAL_ENDPOINTS).
		authed.Get("/v1/local/endpoints", func(w http.ResponseWriter, _ *http.Request) {
			writeJSONOK(w, http.StatusOK, localRouterMulti.List())
		})

		authed.Post("/v1/local/endpoints", func(w http.ResponseWriter, req *http.Request) {
			var in localrouter.LocalEndpoint
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.URL == "" || in.Provider == "" {
				writeJSONErr(w, http.StatusBadRequest, "url and provider required")
				return
			}
			localRouterMulti.Register(&in)
			// Fire an immediate health check so the new endpoint
			// is usable on the next request rather than after the
			// 30s tick.
			go localRouterMulti.CheckHealth(ctx, &in)
			writeJSONOK(w, http.StatusCreated, &in)
		})

		authed.Delete("/v1/local/endpoints/{id}", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			if !localRouterMulti.Remove(id) {
				writeJSONErr(w, http.StatusNotFound, "endpoint not found")
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Post("/v1/local/endpoints/{id}/check", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			ep, err := localRouterMulti.CheckHealthByID(req.Context(), id)
			if err != nil {
				writeJSONErr(w, http.StatusNotFound, "endpoint not found")
				return
			}
			writeJSONOK(w, http.StatusOK, ep)
		})

		// ─── LENS token endpoints (Batch 2 Item 1) ─────
		authed.Get("/v1/workspaces/{wsID}/tokens/balance", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			snap, err := tokenLedger.GetSnapshot(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, snap)
		})

		authed.Get("/v1/workspaces/{wsID}/tokens/history", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
			offset, _ := strconv.Atoi(req.URL.Query().Get("offset"))
			entries, err := tokenLedger.GetHistory(req.Context(), wsID, limit, offset)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, entries)
		})

		// ─── LXC compute credit (two-token split) ──────
		authed.Get("/v1/workspaces/{wsID}/lxc/balance", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			snap, err := dualToken.GetLXCSnapshot(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, snap)
		})

		authed.Post("/v1/workspaces/{wsID}/lxc/convert", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				LXCAmount float64 `json:"lxc_amount"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			res, err := dualToken.ConvertLENStoLXC(req.Context(), wsID, in.LXCAmount)
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, economy.ErrInsufficientLENSFor) {
					status = http.StatusPaymentRequired
				}
				writeJSONErr(w, status, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, res)
		})

		authed.Get("/v1/workspaces/{wsID}/tokens/mining/cache", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := cacheMiner.GetMiningStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		authed.Get("/v1/workspaces/{wsID}/tokens/mining/compute", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := computeMiner.GetWorkspaceStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// ─── Inference node CRUD (compute mining) ──────
		authed.Post("/v1/workspaces/{wsID}/nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in mining.InferenceNode
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in.WorkspaceID = wsID
			created, err := computeMiner.RegisterNode(req.Context(), in)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, created)
		})

		authed.Get("/v1/workspaces/{wsID}/nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodes, err := computeMiner.ListNodes(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, nodes)
		})

		// Node heartbeat (Batch 3 Phase 2). Best-effort UPDATE
		// of last_seen_at + uptime — no auth beyond the
		// already-applied workspace key.
		authed.Post("/v1/workspaces/{wsID}/nodes/{nodeID}/heartbeat", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			var in struct {
				ActiveRequests int64    `json:"active_requests"`
				UptimeSeconds  int64    `json:"uptime_seconds"`
				ModelsLoaded   []string `json:"models_loaded"`
			}
			_ = json.NewDecoder(req.Body).Decode(&in)
			if pool != nil {
				if _, err := pool.Exec(req.Context(), `
					UPDATE inference_nodes
					SET last_seen_at = NOW(), uptime_seconds = $2
					WHERE id = $1`, nodeID, in.UptimeSeconds); err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Delete("/v1/workspaces/{wsID}/nodes/{nodeID}", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if err := computeMiner.DeactivateNode(req.Context(), nodeID); err != nil {
				if errors.Is(err, mining.ErrNodeNotFound) {
					writeJSONErr(w, http.StatusNotFound, "node not found")
					return
				}
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// ─── Cache node registry (Batch 3 Phase 3) ──────
		// Cache nodes register themselves so Lens can route
		// cache reads/writes through their Redis. Earnings flow
		// via the existing CacheMiner pipeline; this endpoint
		// pair just covers registration + heartbeat.
		authed.Post("/v1/workspaces/{wsID}/cache-nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				URL        string  `json:"url"`
				MaxSizeGB  float64 `json:"max_size_gb"`
				NodeSecret string  `json:"node_secret"`
				Share      bool    `json:"share"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.URL == "" {
				writeJSONErr(w, http.StatusBadRequest, "url required")
				return
			}
			if in.MaxSizeGB <= 0 {
				in.MaxSizeGB = 10
			}
			if pool == nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "DB unavailable")
				return
			}
			row := pool.QueryRow(req.Context(), `
				INSERT INTO cache_nodes (workspace_id, url, max_size_gb, node_secret_hash)
				VALUES ($1, $2, $3, $4)
				RETURNING id, created_at`,
				wsID, strings.TrimRight(in.URL, "/"), in.MaxSizeGB, in.NodeSecret)
			var id string
			var createdAt time.Time
			if err := row.Scan(&id, &createdAt); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			// Seed metrics row so heartbeat UPDATEs have a target.
			_, _ = pool.Exec(req.Context(),
				`INSERT INTO cache_node_metrics (node_id) VALUES ($1) ON CONFLICT (node_id) DO NOTHING`, id)
			writeJSONOK(w, http.StatusCreated, map[string]any{
				"id":          id,
				"workspace_id": wsID,
				"url":         in.URL,
				"max_size_gb": in.MaxSizeGB,
				"created_at":  createdAt,
			})
		})

		authed.Delete("/v1/workspaces/{wsID}/cache-nodes/{nodeID}", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if pool == nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "DB unavailable")
				return
			}
			tag, err := pool.Exec(req.Context(),
				`UPDATE cache_nodes SET active = FALSE WHERE id = $1`, nodeID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if tag.RowsAffected() == 0 {
				writeJSONErr(w, http.StatusNotFound, "cache node not found")
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Post("/v1/workspaces/{wsID}/cache-nodes/{nodeID}/heartbeat", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			var in struct {
				Entries int     `json:"entries"`
				SizeMB  float64 `json:"size_mb"`
				HitRate float64 `json:"hit_rate"`
			}
			_ = json.NewDecoder(req.Body).Decode(&in)
			if pool != nil {
				if _, err := pool.Exec(req.Context(), `
					UPDATE cache_nodes SET last_seen_at = NOW() WHERE id = $1`, nodeID); err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				if _, err := pool.Exec(req.Context(), `
					INSERT INTO cache_node_metrics (node_id, entries, size_mb, hit_rate, last_updated)
					VALUES ($1, $2, $3, $4, NOW())
					ON CONFLICT (node_id) DO UPDATE
					SET entries = EXCLUDED.entries,
					    size_mb = EXCLUDED.size_mb,
					    hit_rate = EXCLUDED.hit_rate,
					    last_updated = NOW()`,
					nodeID, in.Entries, in.SizeMB, in.HitRate); err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// ─── Embedding node CRUD (embedding mining) ────
		authed.Post("/v1/workspaces/{wsID}/embedding-nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in mining.EmbeddingNode
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in.WorkspaceID = wsID
			created, err := embeddingMiner.RegisterNode(req.Context(), in)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, created)
		})

		authed.Get("/v1/workspaces/{wsID}/embedding-nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodes, err := embeddingMiner.ListNodes(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, nodes)
		})

		authed.Delete("/v1/workspaces/{wsID}/embedding-nodes/{nodeID}", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if err := embeddingMiner.DeactivateEmbeddingNode(req.Context(), nodeID); err != nil {
				if errors.Is(err, mining.ErrNodeNotFound) {
					writeJSONErr(w, http.StatusNotFound, "node not found")
					return
				}
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Get("/v1/workspaces/{wsID}/tokens/mining/embeddings", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := embeddingMiner.GetStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// ─── Annotation mining (Batch 2 Item 4) ─────────
		authed.Post("/v1/workspaces/{wsID}/annotate/stake", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				Amount float64 `json:"amount"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := annotationMiner.Stake(req.Context(), wsID, in.Amount); err != nil {
				if errors.Is(err, mining.ErrInsufficientBalance) {
					writeJSONErr(w, http.StatusPaymentRequired, err.Error())
					return
				}
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			staked, _ := annotationMiner.GetStake(req.Context(), wsID)
			writeJSONOK(w, http.StatusOK, map[string]float64{"staked": staked})
		})

		authed.Delete("/v1/workspaces/{wsID}/annotate/stake", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			if err := annotationMiner.Unstake(req.Context(), wsID); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Get("/v1/workspaces/{wsID}/annotate/task", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			task, err := annotationMiner.GetPendingTask(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if task == nil {
				writeJSONErr(w, http.StatusNotFound, "no pending tasks")
				return
			}
			writeJSONOK(w, http.StatusOK, task)
		})

		authed.Post("/v1/workspaces/{wsID}/annotate/task/{taskID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			taskID := chi.URLParam(req, "taskID")
			var in struct {
				Decision    string `json:"decision"`
				Confidence  int    `json:"confidence"`
				TimeSpentMs int    `json:"time_spent_ms"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			err := annotationMiner.SubmitAnnotation(req.Context(), mining.Annotation{
				TaskID: taskID, AnnotatorID: wsID,
				Decision: in.Decision, Confidence: in.Confidence, TimeSpentMs: in.TimeSpentMs,
			})
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, mining.ErrDuplicateAnnotation) {
					status = http.StatusConflict
				} else if errors.Is(err, mining.ErrTaskExpired) {
					status = http.StatusGone
				} else if errors.Is(err, mining.ErrInsufficientStake) {
					status = http.StatusPaymentRequired
				}
				writeJSONErr(w, status, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]bool{"ok": true})
		})

		authed.Get("/v1/workspaces/{wsID}/tokens/mining/annotations", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := annotationMiner.GetAnnotatorStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		authed.Get("/v1/workspaces/{wsID}/annotate/stats", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := annotationMiner.GetAnnotatorStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// ─── Pattern mining (Batch 2 Item 5) ────────────
		authed.Post("/v1/workspaces/{wsID}/pattern-mining/opt-in", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			if !cfg.PatternMiningEnabled {
				writeJSONErr(w, http.StatusServiceUnavailable,
					"pattern mining disabled (set LENS_PATTERN_MINING_ENABLED=true)")
				return
			}
			if err := patternMiner.OptIn(req.Context(), wsID); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"opted_in": true})
		})

		authed.Delete("/v1/workspaces/{wsID}/pattern-mining/opt-in", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			if err := patternMiner.OptOut(req.Context(), wsID); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"opted_in": false})
		})

		authed.Get("/v1/workspaces/{wsID}/tokens/mining/patterns", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			c, err := patternMiner.GetContribution(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, c)
		})

		// ─── Token transfers + marketplace + staking ────
		authed.Post("/v1/workspaces/{wsID}/tokens/transfer", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				ToWorkspace string  `json:"to_workspace"`
				Amount      float64 `json:"amount"`
				Description string  `json:"description"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := tokenLedger.Transfer(req.Context(), wsID, in.ToWorkspace, in.Amount, in.Description); err != nil {
				if errors.Is(err, mining.ErrInsufficientBalance) {
					writeJSONErr(w, http.StatusPaymentRequired, err.Error())
					return
				}
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Post("/v1/marketplace/listings", func(w http.ResponseWriter, req *http.Request) {
			var in economy.MarketplaceListing
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.SellerID == "" {
				writeJSONErr(w, http.StatusBadRequest, "seller_id required")
				return
			}
			out, err := marketplace.CreateListing(req.Context(), in)
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, economy.ErrInsufficientBalance) {
					status = http.StatusPaymentRequired
				}
				writeJSONErr(w, status, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, out)
		})

		authed.Post("/v1/marketplace/listings/{id}/buy", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			var in struct {
				BuyerWorkspace string  `json:"buyer_workspace"`
				AmountUSD      float64 `json:"amount_usd"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			trade, err := marketplace.ExecuteTrade(req.Context(), id, in.BuyerWorkspace, in.AmountUSD)
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, economy.ErrListingNotFound) {
					status = http.StatusNotFound
				} else if errors.Is(err, economy.ErrListingNotActive) {
					status = http.StatusGone
				} else if errors.Is(err, economy.ErrInsufficientBalance) {
					status = http.StatusPaymentRequired
				}
				writeJSONErr(w, status, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, trade)
		})

		authed.Delete("/v1/marketplace/listings/{id}", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			wsID := req.URL.Query().Get("workspace_id")
			if wsID == "" {
				writeJSONErr(w, http.StatusBadRequest, "workspace_id query param required")
				return
			}
			if err := marketplace.CancelListing(req.Context(), id, wsID); err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, economy.ErrListingNotFound) {
					status = http.StatusNotFound
				} else if errors.Is(err, economy.ErrNotSeller) {
					status = http.StatusForbidden
				}
				writeJSONErr(w, status, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Get("/v1/marketplace/trades", func(w http.ResponseWriter, req *http.Request) {
			wsID := req.URL.Query().Get("workspace_id")
			if wsID == "" {
				writeJSONErr(w, http.StatusBadRequest, "workspace_id query param required")
				return
			}
			limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
			trades, err := marketplace.GetTrades(req.Context(), wsID, limit)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, trades)
		})

		authed.Post("/v1/workspaces/{wsID}/tokens/stake", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				Amount   float64 `json:"amount"`
				LockDays int     `json:"lock_days"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			pos, err := marketplace.Stake(req.Context(), wsID, in.Amount, in.LockDays)
			if err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, economy.ErrInsufficientBalance) {
					status = http.StatusPaymentRequired
				}
				writeJSONErr(w, status, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, pos)
		})

		authed.Post("/v1/workspaces/{wsID}/tokens/stake/{positionID}/unstake", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			positionID := chi.URLParam(req, "positionID")
			if err := marketplace.Unstake(req.Context(), positionID, wsID); err != nil {
				status := http.StatusBadRequest
				if errors.Is(err, economy.ErrPositionNotFound) {
					status = http.StatusNotFound
				} else if errors.Is(err, economy.ErrStakeLocked) {
					status = http.StatusForbidden
				}
				writeJSONErr(w, status, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Get("/v1/workspaces/{wsID}/tokens/stakes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			positions, err := marketplace.GetStakePositions(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, positions)
		})

		authed.Get("/v1/workspaces/{wsID}/spend/current-month", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			spent, err := spendTracker.CurrentSpend(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]float64{
				"current_month_usd": spent,
			})
		})

		// ─── A/B experiments ────────────────────────────────
		// New experiment system (engine.go). Coexists with the
		// legacy /v1/ab/tests shadow endpoints.
		authed.Post("/v1/workspaces/{wsID}/experiments", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in ab.Experiment
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in.WorkspaceID = wsID
			if err := abEngine.CreateExperiment(req.Context(), &in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, in)
		})

		authed.Get("/v1/workspaces/{wsID}/experiments", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			writeJSONOK(w, http.StatusOK, abEngine.ListExperiments(wsID))
		})

		authed.Get("/v1/workspaces/{wsID}/experiments/{id}", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			exp, ok := abEngine.GetExperiment(id)
			if !ok {
				writeJSONErr(w, http.StatusNotFound, "experiment not found")
				return
			}
			writeJSONOK(w, http.StatusOK, exp)
		})

		authed.Post("/v1/workspaces/{wsID}/experiments/{id}/start", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			if err := abEngine.StartExperiment(req.Context(), id); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]string{"status": "running"})
		})

		authed.Post("/v1/workspaces/{wsID}/experiments/{id}/stop", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			if err := abEngine.StopExperiment(req.Context(), id); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]string{"status": "completed"})
		})

		authed.Get("/v1/workspaces/{wsID}/experiments/{id}/analysis", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			analysis, err := abEngine.AnalyzeExperiment(req.Context(), id)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, analysis)
		})

		authed.Get("/v1/workspaces/{wsID}/experiments/{id}/results", func(w http.ResponseWriter, req *http.Request) {
			// Results live in Postgres directly; the dashboard
			// typically wants `analysis`. We expose the raw stream
			// for ad-hoc debugging — same analysis aggregation but
			// returned per variant.
			id := chi.URLParam(req, "id")
			analysis, err := abEngine.AnalyzeExperiment(req.Context(), id)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, analysis.Variants)
		})

		// ─── Git attribution ────────────────────────────────
		// Per-request rollups served from request_attribution
		// (migration 0017). Complement the legacy /v1/branches
		// endpoints which still serve branch_spend.
		authed.Get("/v1/workspaces/{wsID}/attribution/branches", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			since := parseSinceParam(req.URL.Query().Get("since"))
			limit := 20
			if v := req.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
					limit = n
				}
			}
			rows, err := attrStore.GetCostByBranch(req.Context(), wsID, since, limit)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, rows)
		})

		authed.Get("/v1/workspaces/{wsID}/attribution/branches/{branch}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			branch := chi.URLParam(req, "branch")
			since := parseSinceParam(req.URL.Query().Get("since"))
			stats, err := attrStore.GetBranchStats(req.Context(), wsID, branch, since)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		authed.Get("/v1/workspaces/{wsID}/attribution/prs/{prNumber}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			prNumber := chi.URLParam(req, "prNumber")
			stats, err := attrStore.GetPRStats(req.Context(), wsID, prNumber)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		authed.Get("/v1/workspaces/{wsID}/attribution/summary", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			days := 30
			if v := req.URL.Query().Get("days"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
					days = n
				}
			}
			sum, err := attrStore.GetSummary(req.Context(), wsID, days)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, sum)
		})

		// Quality stats for the per-workspace dashboard. `days`
		// query param caps the rolling window (default 30).
		authed.Get("/v1/workspaces/{wsID}/quality/stats", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			days := 30
			if v := req.URL.Query().Get("days"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
					days = n
				}
			}
			stats, err := qualityScorer.StatsForWorkspace(req.Context(), wsID, days)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// Per-workspace logging policy toggle. Applies immediately —
		// proxy.serve() reads via GetLoggingPolicy on every request and
		// there's no per-request cache of the decision.
		authed.Put("/v1/workspaces/{wsID}/logging", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				LoggingPolicy workspace.LoggingPolicy `json:"logging_policy"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.SetLoggingPolicy(req.Context(), wsID, in.LoggingPolicy); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ws, _ := wsManager.GetWorkspace(wsID)
			writeJSONOK(w, http.StatusOK, map[string]any{
				"ok":             true,
				"logging_policy": ws.LoggingPolicy,
			})
		})

		authed.Get("/v1/attribution/branch", func(w http.ResponseWriter, req *http.Request) {
			branch := req.URL.Query().Get("branch")
			repository := req.URL.Query().Get("repository")
			if branch == "" || repository == "" {
				writeJSONErr(w, http.StatusBadRequest, "branch and repository query params required")
				return
			}
			got, err := branchTracker.GetBranchSpend(req.Context(), branch, repository)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if got == nil {
				writeJSONErr(w, http.StatusNotFound, "branch not found")
				return
			}
			writeJSONOK(w, http.StatusOK, got)
		})

		authed.Get("/v1/attribution/top", func(w http.ResponseWriter, req *http.Request) {
			repository := req.URL.Query().Get("repository")
			if repository == "" {
				writeJSONErr(w, http.StatusBadRequest, "repository query param required")
				return
			}
			limit := 10
			if l := req.URL.Query().Get("limit"); l != "" {
				if n, err := strconv.Atoi(l); err == nil && n > 0 {
					limit = n
				}
			}
			if limit > 50 {
				limit = 50
			}
			top, err := branchTracker.GetTopBranches(req.Context(), repository, limit)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, top)
		})

		authed.Post("/v1/ab/tests", func(w http.ResponseWriter, req *http.Request) {
			var in ab.ABTest
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := abTester.RegisterTest(in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]string{"id": in.ID})
		})

		authed.Get("/v1/ab/tests/{testID}", func(w http.ResponseWriter, req *http.Request) {
			testID := chi.URLParam(req, "testID")
			got, ok := abTester.GetResults(testID)
			if !ok {
				writeJSONErr(w, http.StatusNotFound, "test not found")
				return
			}
			writeJSONOK(w, http.StatusOK, got)
		})

		authed.Get("/v1/sessions/{sessionID}", func(w http.ResponseWriter, req *http.Request) {
			sessionID := chi.URLParam(req, "sessionID")
			s, ok := sessionTracker.GetSession(sessionID)
			if !ok {
				writeJSONErr(w, http.StatusNotFound, "session not found")
				return
			}
			writeJSONOK(w, http.StatusOK, s)
		})

		authed.Get("/v1/sessions/{sessionID}/summary", func(w http.ResponseWriter, req *http.Request) {
			sessionID := chi.URLParam(req, "sessionID")
			summary := sessionTracker.SummariseSession(req.Context(), sessionID)
			if summary.TurnCount == 0 && summary.StartedAt.IsZero() {
				writeJSONErr(w, http.StatusNotFound, "session not found")
				return
			}
			writeJSONOK(w, http.StatusOK, summary)
		})

		authed.Get("/v1/sessions", func(w http.ResponseWriter, req *http.Request) {
			wsID := req.URL.Query().Get("workspace_id")
			if wsID == "" {
				wsID = "default"
			}
			writeJSONOK(w, http.StatusOK, sessionTracker.ListActiveByWorkspace(wsID))
		})

		authed.Post("/v1/batch/submit", func(w http.ResponseWriter, req *http.Request) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "read body: "+err.Error())
				return
			}
			wsID := req.Header.Get("X-Talyvor-Workspace")
			if wsID == "" {
				wsID = "default"
			}
			// Make sure IsEligible sees the batch trigger even when the
			// header — not the body — set it.
			body = ensureBatchFlag(body)
			elig := batchRouter.IsEligible(body, wsID)
			if !elig.Eligible {
				writeJSONErr(w, http.StatusBadRequest, elig.Reason)
				return
			}
			var parsed struct {
				Model    string `json:"model"`
				Messages []struct {
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			_ = json.Unmarshal(body, &parsed)
			prompt := ""
			for _, m := range parsed.Messages {
				var s string
				if json.Unmarshal(m.Content, &s) == nil {
					prompt += s
				}
			}
			job, err := batchRouter.Submit(req.Context(), wsID, parsed.Model, prompt, body)
			if err != nil {
				writeJSONErr(w, http.StatusBadGateway, err.Error())
				return
			}
			writeJSONOK(w, http.StatusAccepted, map[string]any{
				"request_id":           job.RequestID,
				"batch_id":             job.ID,
				"status":               string(job.Status),
				"estimated_completion": "within 24 hours",
				"cost_reduction":       "50%",
			})
		})

		authed.Get("/v1/batch/status/{requestID}", func(w http.ResponseWriter, req *http.Request) {
			requestID := chi.URLParam(req, "requestID")
			job := batchRouter.GetJobByRequestID(requestID)
			if job == nil {
				writeJSONErr(w, http.StatusNotFound, "batch job not found")
				return
			}
			writeJSONOK(w, http.StatusOK, job)
		})

		authed.Get("/v1/batch/jobs", func(w http.ResponseWriter, req *http.Request) {
			// workspace_id filtering happens client-side for now — the
			// in-memory list doesn't index by workspace.
			writeJSONOK(w, http.StatusOK, batchRouter.ListJobs())
		})

		// Eval pipeline — test cases, suite runs, and history. RunSuite
		// is synchronous from the caller's perspective; up to 10 cases
		// execute concurrently inside the handler.
		authed.Post("/v1/eval/cases", func(w http.ResponseWriter, req *http.Request) {
			var in eval.TestCase
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			created, err := evalPipeline.AddTestCase(req.Context(), in)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, created)
		})

		authed.Get("/v1/eval/cases", func(w http.ResponseWriter, req *http.Request) {
			wsID := req.URL.Query().Get("workspace_id")
			if wsID == "" {
				wsID = "default"
			}
			var tags []string
			if t := req.URL.Query().Get("tags"); t != "" {
				tags = strings.Split(t, ",")
			}
			cases, err := evalPipeline.ListTestCases(req.Context(), wsID, tags)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, cases)
		})

		authed.Post("/v1/eval/run", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				WorkspaceID string   `json:"workspace_id"`
				Tags        []string `json:"tags"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.WorkspaceID == "" {
				in.WorkspaceID = "default"
			}
			summary, err := evalPipeline.RunSuite(req.Context(), in.WorkspaceID, in.Tags)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, summary)
		})

		authed.Get("/v1/eval/runs/{runID}", func(w http.ResponseWriter, req *http.Request) {
			runID := chi.URLParam(req, "runID")
			summary, err := evalPipeline.GetRun(req.Context(), runID)
			if err != nil {
				writeJSONErr(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, summary)
		})

		authed.Get("/v1/eval/runs/{runID}/results", func(w http.ResponseWriter, req *http.Request) {
			runID := chi.URLParam(req, "runID")
			results, err := evalPipeline.GetResults(req.Context(), runID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, results)
		})

		authed.Get("/v1/eval/runs", func(w http.ResponseWriter, req *http.Request) {
			wsID := req.URL.Query().Get("workspace_id")
			if wsID == "" {
				wsID = "default"
			}
			runs, err := evalPipeline.ListRuns(req.Context(), wsID, 10)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, runs)
		})

		// Guardrails: per-workspace safety policy + pre-flight check.
		// Policy changes apply immediately; the engine reads on every
		// proxy request and there is no per-request policy cache.
		authed.Get("/v1/guardrails/policy", func(w http.ResponseWriter, req *http.Request) {
			wsID := req.URL.Query().Get("workspace_id")
			if wsID == "" {
				wsID = "default"
			}
			writeJSONOK(w, http.StatusOK, guardrailsEngine.GetPolicy(wsID))
		})

		authed.Put("/v1/guardrails/policy", func(w http.ResponseWriter, req *http.Request) {
			var in guardrails.GuardrailPolicy
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.WorkspaceID == "" {
				in.WorkspaceID = "default"
			}
			guardrailsEngine.SetPolicy(req.Context(), in.WorkspaceID, in)
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Post("/v1/guardrails/check", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				Prompt      string `json:"prompt"`
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.WorkspaceID == "" {
				in.WorkspaceID = "default"
			}
			result := guardrailsEngine.Check(req.Context(), in.WorkspaceID, in.Prompt, nil)
			writeJSONOK(w, http.StatusOK, result)
		})

		// Audit export — synchronous download. The exporter streams rows
		// straight to the http.ResponseWriter so a 100k-record CSV never
		// materialises in memory. Format defaults to JSON; query params
		// drive the WHERE filters and the LIMIT cap.
		authed.Get("/v1/audit/export", func(w http.ResponseWriter, req *http.Request) {
			q := req.URL.Query()
			format := audit.ExportFormat(q.Get("format"))
			if format == "" {
				format = audit.FormatJSON
			}
			filter := audit.ExportFilter{
				WorkspaceID: q.Get("workspace_id"),
				Team:        q.Get("team"),
				Provider:    q.Get("provider"),
			}
			if v := q.Get("start"); v != "" {
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					filter.StartTime = t
				}
			}
			if v := q.Get("end"); v != "" {
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					filter.EndTime = t
				}
			}
			if v := q.Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					filter.MaxRecords = n
				}
			}
			// Pick MIME + filename extension up front so we can emit
			// Content-Disposition before any body bytes are written.
			var ct, ext string
			switch format {
			case audit.FormatCSV:
				ct, ext = "text/csv", "csv"
			case audit.FormatNDJSON:
				ct, ext = "application/x-ndjson", "ndjson"
			default:
				ct, ext = "application/json", "json"
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="audit-%s.%s"`, time.Now().UTC().Format("2006-01-02"), ext))
			w.WriteHeader(http.StatusOK)
			if _, err := auditExporter.Export(req.Context(), filter, format, w); err != nil {
				// Headers are already committed; surface the failure in the
				// structured log so compliance ops can correlate later.
				logger.Warn("audit: export failed mid-stream", slog.String("err", err.Error()))
			}
		})

		authed.Post("/v1/audit/webhook", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				WebhookURL string             `json:"webhook_url"`
				Filter     audit.ExportFilter `json:"filter"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.WebhookURL == "" {
				writeJSONErr(w, http.StatusBadRequest, "webhook_url required")
				return
			}
			// Fire-and-forget so the caller doesn't block on a slow SIEM.
			// Use a fresh context detached from the request so cancellation
			// of the HTTP connection doesn't kill the export mid-flight.
			go func(filter audit.ExportFilter, url string) {
				if err := auditExporter.ExportWebhook(context.Background(), url, filter); err != nil {
					logger.Warn("audit: webhook export failed",
						slog.String("url", url),
						slog.String("err", err.Error()),
					)
				}
			}(in.Filter, in.WebhookURL)
			writeJSONOK(w, http.StatusAccepted, map[string]any{"ok": true, "message": "export started"})
		})

		// API-key pool: enterprise customers attach multiple keys per
		// provider to escape per-key rate limits. Raw key material is
		// kept in memory only; the pool's Stats API never returns it.
		authed.Post("/v1/api/keys/pool", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				Provider  string `json:"provider"`
				Key       string `json:"key"`
				Alias     string `json:"alias"`
				RateLimit int    `json:"rate_limit"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			pk, err := keyPool.Add(in.Provider, in.Key, in.Alias, in.RateLimit)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]string{
				"id":       pk.ID,
				"provider": pk.Provider,
				"alias":    pk.Alias,
			})
		})

		authed.Get("/v1/api/keys/pool", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, keyPool.Stats())
		})

		authed.Delete("/v1/api/keys/pool/{keyID}", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "keyID")
			if !keyPool.Remove(id) {
				writeJSONErr(w, http.StatusNotFound, "key not found")
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// Fallback chain inspection and override. The router is in-memory;
		// updates here are not persisted — restarting the binary resets
		// chains to the defaults.
		authed.Get("/v1/api/fallback/chains", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, fallbackRouter.AllChains())
		})

		authed.Put("/v1/api/fallback/chains/{provider}", func(w http.ResponseWriter, req *http.Request) {
			provider := chi.URLParam(req, "provider")
			var targets []fallback.FallbackTarget
			if err := json.NewDecoder(req.Body).Decode(&targets); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			fallbackRouter.SetChain(provider, targets)
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// Prompt management — named, versioned prompts that teams edit
		// without redeploys. Every write goes through the Manager so the
		// in-memory cache stays consistent with the DB.
		authed.Post("/v1/prompts", func(w http.ResponseWriter, req *http.Request) {
			var in prompts.Prompt
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			created, err := promptManager.Create(req.Context(), in)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, created)
		})

		authed.Get("/v1/prompts", func(w http.ResponseWriter, req *http.Request) {
			wsID := req.URL.Query().Get("workspace_id")
			if wsID == "" {
				wsID = "default"
			}
			list, err := promptManager.List(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		authed.Get("/v1/prompts/{name}", func(w http.ResponseWriter, req *http.Request) {
			name := chi.URLParam(req, "name")
			wsID := req.URL.Query().Get("workspace_id")
			if wsID == "" {
				wsID = "default"
			}
			pr, err := promptManager.Get(req.Context(), name, wsID)
			if err != nil {
				writeJSONErr(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, pr)
		})

		authed.Put("/v1/prompts/{name}", func(w http.ResponseWriter, req *http.Request) {
			name := chi.URLParam(req, "name")
			var in struct {
				Content     string `json:"content"`
				Description string `json:"description"`
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.WorkspaceID == "" {
				in.WorkspaceID = "default"
			}
			updated, err := promptManager.Update(req.Context(), name, in.WorkspaceID, in.Content, in.Description)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, updated)
		})

		authed.Get("/v1/prompts/{name}/history", func(w http.ResponseWriter, req *http.Request) {
			name := chi.URLParam(req, "name")
			wsID := req.URL.Query().Get("workspace_id")
			if wsID == "" {
				wsID = "default"
			}
			hist, err := promptManager.History(req.Context(), name, wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, hist)
		})

		authed.Post("/v1/prompts/{name}/rollback", func(w http.ResponseWriter, req *http.Request) {
			name := chi.URLParam(req, "name")
			var in struct {
				Version     int    `json:"version"`
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.WorkspaceID == "" {
				in.WorkspaceID = "default"
			}
			rolled, err := promptManager.Rollback(req.Context(), name, in.WorkspaceID, in.Version)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, rolled)
		})

		authed.Get("/v1/prompts/{name}/diff", func(w http.ResponseWriter, req *http.Request) {
			name := chi.URLParam(req, "name")
			wsID := req.URL.Query().Get("workspace_id")
			if wsID == "" {
				wsID = "default"
			}
			fromV, _ := strconv.Atoi(req.URL.Query().Get("from"))
			toV, _ := strconv.Atoi(req.URL.Query().Get("to"))
			if fromV <= 0 || toV <= 0 {
				writeJSONErr(w, http.StatusBadRequest, "from and to query params required (positive integers)")
				return
			}
			d, err := promptManager.Diff(req.Context(), name, wsID, fromV, toV)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, d)
		})

		authed.Post("/v1/feedback", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				PromptHash string                 `json:"prompt_hash"`
				Signal     quality.FeedbackSignal `json:"signal"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if in.PromptHash == "" || in.Signal == "" {
				writeJSONErr(w, http.StatusBadRequest, "prompt_hash and signal are required")
				return
			}
			if err := qualityScorer.RecordFeedback(req.Context(), in.PromptHash, in.Signal); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server listening", slog.String("addr", cfg.ListenAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			return err
		}
	}

	// Graceful drain (Upgrade 7): mark this instance draining so /readyz starts
	// returning 503 and the load balancer pulls it from rotation; let in-flight
	// requests finish via srv.Shutdown; then deregister and close the gossip
	// subscription. When HA is disabled the registry/breaker calls are no-ops
	// and the drain timeout stays at the original 15s, so shutdown is unchanged.
	drainTimeout := 15 * time.Second
	if cfg.HAEnabled {
		drainTimeout = cfg.HADrainTimeout
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if err := haComps.registry.SetDraining(drainCtx); err != nil {
		logger.Warn("ha: set draining failed", slog.String("err", err.Error()))
	}
	if cfg.HAEnabled {
		// Give the load balancer a moment to observe the 503 from /readyz
		// before we stop accepting connections, so no new request is dropped.
		select {
		case <-time.After(2 * time.Second):
		case <-drainCtx.Done():
		}
	}

	if err := srv.Shutdown(drainCtx); err != nil {
		return err
	}

	if err := haComps.registry.Deregister(context.Background()); err != nil {
		logger.Warn("ha: deregister failed", slog.String("err", err.Error()))
	}
	if err := haComps.breaker.Close(); err != nil {
		logger.Warn("ha: breaker close failed", slog.String("err", err.Error()))
	}

	return nil
}

func writeJSONOK(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	writeJSONOK(w, status, map[string]string{"error": msg})
}

// parseSinceParam turns a `?since=<RFC3339>` query value into a
// time.Time. Empty / unparseable input returns the zero value,
// which the attribution store treats as "all time".
func parseSinceParam(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// ensureBatchFlag stamps batch_eligible:true into the body so the
// BatchRouter's body-only IsEligible sees the trigger that arrived via
// the X-Talyvor-Batch header.
func ensureBatchFlag(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["batch_eligible"] = true
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

func initTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("talyvor-lens"),
		),
	)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	// Composite propagator extracts/injects both W3C traceparent (the
	// trace ID we care about) and W3C baggage (key-value context that
	// upstream apps may attach to requests).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp, nil
}
