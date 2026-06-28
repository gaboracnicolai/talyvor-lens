package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	"github.com/talyvor/lens/internal/backpressure"
	"github.com/talyvor/lens/internal/batch"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/budget"
	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/compat"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/controlplane"
	"github.com/talyvor/lens/internal/costanomaly"
	"github.com/talyvor/lens/internal/dashboard"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/dbrouting"
	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/distillattrib"
	"github.com/talyvor/lens/internal/distillpreview"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/eval"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/forecast"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/keypool"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/mcp"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/oracle"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/internal/prompts"
	"github.com/talyvor/lens/internal/proxy"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/internal/ratelimit"
	"github.com/talyvor/lens/internal/retry"
	"github.com/talyvor/lens/internal/roi"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/routing"
	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/status"
	"github.com/talyvor/lens/internal/templates"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/warmer"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/internal/worktier"
	"github.com/talyvor/lens/migrations"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	// Subcommand dispatch. Only an explicit "migrate" diverts; with no
	// subcommand (or any other arg, as before) the process starts the gateway
	// server exactly as it always has — the default entrypoint is unchanged.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := runMigrate(); err != nil {
			slog.Error("migrate failed", slog.String("err", err.Error()))
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("startup failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// runMigrate applies the embedded SQL migrations and exits. It reads the SAME
// LENS_DATABASE_URL the server uses, but deliberately does NOT call
// config.Load() — that validates Redis/NATS/provider keys a migration Job has
// no business needing. Idempotent (already-applied versions are skipped) and
// fail-loud (a migration error returns non-nil → os.Exit(1) so the K8s Job
// fails and the rollout is held back from an unmigrated DB).
func runMigrate() error {
	level := os.Getenv("LENS_LOG_LEVEL")
	if level == "" {
		level = "info"
	}
	slog.SetDefault(newLogger(level))

	rawDBURL := os.Getenv("LENS_DATABASE_URL")
	if rawDBURL == "" {
		return dbmigrate.ErrNoDatabaseURL
	}
	// Apply the same sslmode default as the main server so the migration
	// connection is also encrypted. The operator can override by either
	// embedding sslmode= in LENS_DATABASE_URL or by setting LENS_DB_SSL_MODE.
	migrateSSLMode := os.Getenv("LENS_DB_SSL_MODE")
	if migrateSSLMode == "" {
		migrateSSLMode = "require"
	}
	// Validate before injecting — without this, an invalid value like
	// LENS_DB_SSL_MODE=badinput would pass silently and produce a confusing
	// pgx connect error instead of a clear startup failure.
	if err := config.ValidateDBSSLMode(migrateSSLMode); err != nil {
		return err
	}
	dbURL := injectSSLMode(rawDBURL, migrateSSLMode)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("migrate: connect: %w", err)
	}
	defer func() {
		// context.Background() would hang indefinitely if the network is
		// degraded; use a short bounded timeout so the process can exit cleanly.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = conn.Close(closeCtx)
	}()

	applied, err := dbmigrate.Run(ctx, conn, migrations.FS)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		slog.Info("migrate: database already up to date")
	} else {
		slog.Info("migrate: applied migrations",
			slog.Int("count", len(applied)),
			slog.Any("versions", applied))
	}
	return nil
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
	// redisTLSActive is true when either:
	//   (a) the URL uses rediss:// — go-redis already set TLSConfig, or
	//   (b) LENS_REDIS_TLS=true — operator forces TLS on a redis:// URL.
	redisTLSActive := redisOpts.TLSConfig != nil || cfg.RedisTLS
	if redisTLSActive {
		applyRedisTLS(redisOpts, cfg.RedisTLSSkipVerify)
		logger.Info("redis TLS enabled", slog.Bool("skip_verify", cfg.RedisTLSSkipVerify))
		if cfg.RedisTLSSkipVerify {
			logger.Warn("redis TLS certificate verification is disabled (LENS_REDIS_TLS_SKIP_VERIFY=true)" +
				" — only appropriate for self-signed certs in controlled environments")
		}
	} else {
		logger.Warn("redis TLS is disabled — connection is unencrypted;" +
			" use a rediss:// URL or set LENS_REDIS_TLS=true for production")
	}
	redisClient := redis.NewClient(redisOpts)
	defer func() { _ = redisClient.Close() }()

	if err := redisClient.Ping(ctx).Err(); err != nil {
		logger.Warn("redis ping failed", slog.String("err", err.Error()))
	}

	// Inject sslmode before parsing so every pool connection is encrypted.
	// injectSSLMode is a no-op when the operator already set sslmode= in the URL.
	dbURL := injectSSLMode(cfg.DatabaseURL, cfg.DBSSLMode)
	if cfg.DBSSLMode == "disable" {
		logger.Warn("postgres TLS is disabled — connection is unencrypted; set LENS_DB_SSL_MODE=require for production")
	} else {
		logger.Info("postgres TLS", slog.String("sslmode", cfg.DBSSLMode))
	}

	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return fmt.Errorf("pgxpool parse config: %w", err)
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns
	if cfg.DBPgBouncer {
		// PgBouncer in transaction mode doesn't support the extended query
		// protocol (prepared statements). Simple protocol is compatible with
		// all pooling modes and has negligible overhead for OLTP queries.
		poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		logger.Warn("postgres ping failed", slog.String("err", err.Error()))
	}

	// U8/U9 read-replica: an OPTIONAL second pool for analytics/display reads.
	// It stays nil unless LENS_DB_REPLICA_URL is set AND the replica pings OK at
	// boot (OpenReplica degrades every failure mode to nil + a WARN — never a
	// crash). Money/authz/transaction reads NEVER receive this handle; only the
	// recon-confirmed analytics readers are wired through dbrouting.ReadPool.
	replicaPool := dbrouting.OpenReplica(ctx, cfg.DBReplicaURL, dbrouting.ReplicaOpts{
		MaxConns: cfg.DBMaxConns, MinConns: cfg.DBMinConns, PgBouncer: cfg.DBPgBouncer, Log: logger,
	})
	if replicaPool != nil {
		defer replicaPool.Close()
	}
	// U8/U9 lag observability: sample replica replay lag into the Prometheus
	// gauge + surface it on /healthz. A no-op when replicaPool is nil (gauge
	// stays 0, health entry reports a healthy "no replica configured").
	replicaLagMonitor := dbrouting.NewLagMonitor(replicaPool, metrics.SetReplicaLagSeconds, 0, logger)
	replicaLagMonitor.Start(ctx)

	nc, err := connectNATS(cfg.NatsURL, cfg, logger)
	if err != nil {
		return err
	}
	defer nc.Close()

	exactCache := cache.NewExactCache(redisClient, cfg.MaxCacheTTL)
	openAIEmbedder := embedder.NewOpenAIEmbedder(cfg.OpenAIAPIKey, cfg.EmbeddingModel, cfg.EmbeddingBaseURL)
	semanticCache := cache.NewSemanticCache(pool, openAIEmbedder, cfg.SemanticThreshold, cfg.SemanticCacheRetention)
	promptCompressor := compressor.New()
	modelRouter := router.New()
	piiDetector := pii.New()
	alertManager := alerts.New(pool, nc, nil) // rules loaded from DB in a future iteration
	alertManager.StartMonitor(ctx)
	templateDetector := templates.New(pool)
	qualityScorer := quality.New(pool)
	abEngine := ab.NewEngine(pool)
	abEngine.RunAutoCompleteLoop(ctx, time.Hour)
	branchTracker := attribution.New() // header attribution only; branch_spend writes retired (#157)
	// One shared bound across all post-serve observational writers
	// (attribution + pattern capture), so their combined claim on the DB
	// pool is capped. Drop-on-overflow by design: under overload these
	// writers otherwise churn pool connections until PgBouncer's
	// max_client_conn is exhausted (#122). nil when set to 0 (bound off).
	obsLimiter := backpressure.New(cfg.ObsWriteMaxConcurrent)

	attrStore := attribution.NewStore(pool)
	attrStore.SetWriteLimiter(obsLimiter)
	budgetStore := budgets.NewStore(pool)
	budgetService := budgets.NewService(budgetStore)
	// Predictive cost forecasting (Upgrade 20). Read-only analytics over
	// token_events + budgets; cached, off the request hot path.
	forecaster := forecast.New(forecast.NewStore(dbrouting.ReadPool(pool, replicaPool)))
	// Cross-sectional cost anomaly detection (Upgrade 21). Read-only,
	// cached, off the hot path. Distinct from the temporal anomaly.Detector.
	costAnomalyStore := costanomaly.NewStore(dbrouting.ReadPool(pool, replicaPool))
	costAnomalyDetector := costanomaly.New(costAnomalyStore)
	// Executive ROI reporting (Upgrade 24). Read-only orchestration of
	// budgets + forecast + costanomaly + attribution; cached, off the hot
	// path. Per-engineer breakdown is opt-in (sensitive — see config).
	roiReporter := roi.New(costAnomalyStore, budgetStore, budgetStore, forecaster, costAnomalyDetector, attrStore, roi.Config{
		IncludeEngineerBreakdown: cfg.ROIIncludeEngineerBreakdown,
	})
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
	lr.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 5*time.Second))

	// Multi-endpoint local router (additive, see internal/localrouter/multi.go).
	// Parses LENS_LOCAL_ENDPOINTS; if empty, the registry stays empty and
	// the admin API can register endpoints dynamically at runtime.
	localRouterMulti := localrouter.NewRouterFromConfig(pool, cfg.LocalEndpoints)
	localRouterMulti.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 5*time.Second))
	if cfg.NodeTLSSkipVerify {
		logger.Warn("node TLS certificate verification is disabled (LENS_NODE_TLS_SKIP_VERIFY=true)" +
			" — only appropriate for self-signed certs on controlled private networks")
	}
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
	// Attribute eval spend through the normal cost ledger (feature/modality
	// "eval") so eval runs show up in budgets/forecasts/reports, and run the
	// scheduler for cadence-based dataset evals (both off the request hot path).
	evalPipeline.SetSpendRecorder(alertManager)
	// eval scheduler and anomaly monitor goroutines are launched after setupHA
	// below so they can be wrapped with leader election.
	anomalyDetector := anomaly.New(dbrouting.ReadPool(pool, replicaPool))
	statusPage := status.New(pool, redisClient, nc, "0.1.0")
	go statusPage.StartCacher(ctx, 60*time.Second)
	// Model-catalog runtime overrides (Upgrade 16): an operator can add or
	// reprice models without a rebuild via LENS_MODEL_CATALOG_OVERRIDES (a
	// JSON array of catalog.Model), layered on top of the embedded default.
	if raw := os.Getenv("LENS_MODEL_CATALOG_OVERRIDES"); raw != "" {
		var overrides []catalog.Model
		if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
			logger.Warn("catalog: invalid LENS_MODEL_CATALOG_OVERRIDES", slog.String("err", err.Error()))
		} else {
			catalog.LoadOverrides(overrides)
			logger.Info("catalog: applied model overrides", slog.Int("count", len(overrides)))
		}
	}

	guardrailsEngine := guardrails.New(piiDetector, injectionDetector)
	// Output-stage guardrails (Upgrade 13) are OFF by default; when off the
	// input guardrails behave exactly as today and no output guardrails run.
	guardrailsEngine.SetOutputEnabled(cfg.GuardrailsEnabled)
	// #189: persist custom per-workspace guardrail policies (guardrail_policies,
	// 0014) so they survive restart and propagate across replicas. SetStore wires
	// the pool; Load seeds the cache at boot (on failure the engine serves
	// defaultPolicy — PII redact + injection block stay ON — and flags Degraded());
	// StartRefresh bounds cross-replica staleness, not leader-gated (each replica
	// refreshes its own cache).
	guardrailsEngine.SetStore(pool)
	if err := guardrailsEngine.Load(ctx); err != nil {
		logger.Warn("guardrails: initial policy load failed (serving defaults, Degraded)", slog.String("err", err.Error()))
	}
	guardrailsEngine.StartRefresh(ctx, cfg.GuardrailsReloadInterval)

	l := learner.New(nc, pool)
	go l.StartBackground(ctx)

	cacheWarmer := warmer.New(pool, l, exactCache, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey)
	go cacheWarmer.Start(ctx, 1*time.Hour)

	// Semantic-cache retention sweeper: deletes prompt_embeddings rows unused
	// within the retention window (their updated_at is bumped on every hit, so
	// the timer resets on use). Bounds table + ivfflat-index growth. Inert when
	// SemanticCacheRetention <= 0 (StartSweeper logs and returns).
	go semanticCache.StartSweeper(ctx, cfg.SemanticCacheSweepInterval)

	p := proxy.New(exactCache, semanticCache, openAIEmbedder, promptCompressor, modelRouter, piiDetector, alertManager, templateDetector, qualityScorer, branchTracker, wsManager, lr, injectionDetector, budgetEnforcer, batchRouter, sessionTracker, promptManager, fallbackRouter, keyPool, auditExporter, guardrailsEngine, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey, cfg.GoogleAPIKey, l)
	// Upgraded per-request attribution store (Upgrade Batch 1 / Item 3).
	// Wired as a setter so the existing proxy.New signature stays put.
	p.SetAttributionStore(attrStore)
	// DISTILL request-path integration (stage 3, PR #2). The isolated
	// distill-worker subprocess converts an opted-in request's document to
	// Markdown before the model call; a Redis-backed conversion cache avoids
	// re-converting the same document. Inert until a workspace's DistillPolicy
	// is enabled (default disabled).
	p.SetDistiller(
		&distill.ProcessIsolator{WorkerBin: cfg.DistillWorkerBin},
		cache.NewDistillCache(redisClient, cfg.MaxCacheTTL),
		// Cross-tenant distill-share consent gate (S0). Default private: the
		// distill cache is wsID-scoped unless LENS_DISTILL_POOLABLE_ENABLED is on
		// AND both the owner and requester have distill_poolable=true (PUT
		// /v1/workspaces/{wsID}/distill-poolable). A SEPARATE consent from
		// cache_poolable — distill artifacts are document-derived.
		cache_pooling.New(
			func() bool { return cfg.DistillPoolableEnabled },
			wsManager.GetDistillPoolable,
		),
		// S1 distill attribution sink (MINT-FREE): records consented cross-tenant
		// pooled-distill serves into distill_serve_attribution (migration 0052).
		// No ledger, no read endpoint yet — write-only. Inert unless a serve is
		// actually consented cross-tenant (requires the distill_poolable flags on).
		distillattrib.NewStore(pool),
	)
	// Phase-2 Stage 2.0 shared-cache governance gate (exact cache). Read-only:
	// reads the global switch + each workspace's cache_poolable opt-in, mutates
	// nothing. Inert by default — pooling stays off until LENS_CACHE_POOLABLE_ENABLED
	// is set AND a workspace opts in (PUT /v1/workspaces/{wsID}/cache-poolable).
	p.SetPoolGate(cache_pooling.New(
		func() bool { return cfg.CachePoolableEnabled },
		wsManager.GetCachePoolable,
	))
	// Per-team / per-sprint budget governance (Upgrade 19). Seed the
	// in-memory snapshot from token_events, refresh it periodically, then
	// wire the gate into the proxy hot path. Load is best-effort — a cold
	// start simply begins with zero budgets until the first refresh.
	if err := budgetService.Load(ctx); err != nil {
		logger.Warn("budgets: initial load failed", slog.String("err", err.Error()))
	}
	budgetService.StartRefresh(ctx)
	// U7b: bound cross-replica staleness of workspace config (logging policy +
	// the cache-pooling privacy flag) — every replica rebuilds its in-memory
	// workspace cache from Postgres on this interval (build-then-swap LoadAll).
	// NOT leader-gated: each replica must refresh its OWN cache. ctx is the run
	// lifecycle context (cancelled on shutdown → the ticker goroutine exits).
	// Past WorkspaceMaxStaleness without a successful reload, the consent/privacy
	// accessors fail closed (pause pooling / clamp logging) until the DB recovers.
	wsManager.SetMaxStaleness(cfg.WorkspaceMaxStaleness)
	wsManager.StartRefresh(ctx, cfg.WorkspaceReloadInterval)
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
	// Same shared observational-write bound as attribution/pattern-capture:
	// the async last_used_at updater is goroutine-per-authenticated-request
	// and must not be able to pile up against the pool under overload (#174).
	keyStore.SetWriteLimiter(obsLimiter)
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

	// Parse or generate the EC P-256 JWT signing key (ES256).
	// Production deployments must set LENS_JWT_PRIVATE_KEY so that tokens
	// survive restarts and are shared across HA instances. When the env var
	// is absent we generate an ephemeral key — acceptable for a single-node
	// dev run but useless for HA (each instance would mint unverifiable tokens).
	var jwtKey *ecdsa.PrivateKey
	if cfg.JWTPrivateKey != "" {
		var keyErr error
		jwtKey, keyErr = auth.ParseECPrivateKeyPEM(cfg.JWTPrivateKey)
		if keyErr != nil {
			logger.Error("failed to parse LENS_JWT_PRIVATE_KEY", slog.String("error", keyErr.Error()))
			os.Exit(1)
		}
		logger.Info("JWT signing: loaded EC P-256 key from LENS_JWT_PRIVATE_KEY",
			slog.String("kid", auth.JWTKid))
	} else {
		var keyErr error
		jwtKey, keyErr = auth.GenerateECKey()
		if keyErr != nil {
			logger.Error("failed to generate ephemeral JWT key", slog.String("error", keyErr.Error()))
			os.Exit(1)
		}
		logger.Warn("JWT signing: using ephemeral EC P-256 key — tokens will not survive a restart; " +
			"set LENS_JWT_PRIVATE_KEY for production")
	}

	// Auth manager — unified JWT + workspace-key + global-key
	// authenticator. Coexists with the legacy auth.AuthMiddleware
	// (which is still mounted below for backward compat); new
	// /v1/auth/* routes use authManager directly.
	authManager := auth.NewManager(os.Getenv("LENS_API_KEY"), jwtKey, keyStore, tenantStore)

	// requireAdmin (admin-gate) is now a package-level helper in
	// authz_admin_handlers.go — it takes authManager explicitly so it is
	// testable over HTTP. Used for /metrics, /ha/status, and the #153
	// global-config write routes.

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

	// Singleton background jobs — wrapped with leader election so exactly one
	// instance runs each when HA is enabled; runs directly when disabled.
	go haComps.leader.Run(ctx, "eval-scheduler", 30*time.Second, func(lctx context.Context) {
		evalPipeline.StartScheduler(lctx, time.Minute)
	})
	go haComps.leader.Run(ctx, "anomaly-monitor", 30*time.Second, func(lctx context.Context) {
		anomalyDetector.StartMonitor(lctx, nc, 1*time.Hour)
	})

	// U14 audit-trail integrity — leader-only singleton jobs (exactly one instance).
	// Both DEFAULT OFF: the token_events retention sweeper is inert unless
	// LENS_AUDIT_RETENTION > 0, and the off-box export is inert unless
	// LENS_AUDIT_EXPORT_URL is set — StartSweeper/StartLoop return immediately when
	// their feature is disabled, so leader election spins on a no-op.
	auditRetention := audit.NewRetention(pool, cfg.AuditRetention,
		cfg.AuditRequireExportBeforePrune, cfg.AuditExportURL != "")
	go haComps.leader.Run(ctx, "audit-retention", 30*time.Second, func(lctx context.Context) {
		auditRetention.StartSweeper(lctx, time.Hour)
	})
	auditExport := audit.NewScheduledExport(pool, cfg.AuditExportURL)
	go haComps.leader.Run(ctx, "audit-export", 30*time.Second, func(lctx context.Context) {
		auditExport.StartLoop(lctx, cfg.AuditExportInterval)
	})

	// Control-plane — stateless reconciler (leader-only) + syncer (every instance).
	//
	// Reconciler: marks stale nodes inactive in Postgres, builds a NodeSnapshot,
	// and publishes it to Redis on every tick.
	//
	// NodeSyncer: every Lens instance reads the Redis snapshot on every tick and
	// syncs live inference nodes into localRouterMulti so the proxy's smart
	// endpoint selection (least-loaded / lowest-latency) picks from the live
	// mining fleet rather than only statically configured endpoints.
	// HeartbeatStore buffers heartbeats in Redis so any Lens instance can
	// receive a heartbeat and the Reconciler on any instance (after leader
	// election) sees current liveness state immediately — the xDS HA property.
	cpHB := controlplane.NewHeartbeatStore(redisClient)
	cpStore := controlplane.NewNodeStore(pool, cpHB)
	const cpReconcileInterval = 30 * time.Second
	cpPublisher := controlplane.NewPublisher(redisClient, cpReconcileInterval)
	cpReconciler := controlplane.NewReconciler(cpStore, cpPublisher)
	go haComps.leader.Run(ctx, "node-reconciler", cpReconcileInterval, func(lctx context.Context) {
		cpReconciler.Run(lctx, cpReconcileInterval)
	})
	cpSyncer := controlplane.NewNodeSyncer(cpPublisher, localRouterMulti)
	go cpSyncer.Run(ctx, 30*time.Second)

	// LENS token mining ledger + cache-mining engine (Batch 2 Item 1).
	tokenLedger := mining.NewLedgerStore(pool)
	// U6 Sybil floor: wire the verified-to-earn gate UNCONDITIONALLY at the
	// ledger chokepoint — a safety restriction must not be liftable by the
	// economy toggle (mirrors the LXC-fiat unconditional wiring). Every
	// mint-type credit now requires a verified-to-earn workspace; conservation
	// credits pass through. nil-safe for tests (they construct ledgers without it).
	tokenLedger.SetMintVerifier(earnverify.New())
	// U6 PR2: the per-identity mint rate cap — the universal steady-state bound on
	// Sybil wash yield. Wired UNCONDITIONALLY (a safety restriction the economy
	// kill must not lift, like the verifier). Default 1000 LENS/24h; 0 = off.
	tokenLedger.SetMintRateCap(cfg.MintRateCapLENS24h, 24*time.Hour)
	cacheMiner := mining.NewCacheMiner(tokenLedger, cfg.CacheSharingEnabled)
	_ = cacheMiner // hooks into the cache-hit path in a follow-up wire-up

	// Phase-2 Stage 2.1 Pool-B royalty mint: a SERVED cross-tenant pooled hit
	// mints s × avoided_COGS to the contributing tenant, exactly once per
	// serving request (UNIQUE(request_id) claim + CreditTx in one tx — extends
	// the existing FOR UPDATE ledger discipline, no advisory lock). Inert by
	// default: with LENS_POOL_ROYALTY_MINTING_ENABLED=false (the default) the
	// Minter no-ops before touching the DB, and pooled hits serve exactly as
	// Stage 2.0 left them.
	royaltyMinter := poolroyalty.NewMinter(
		pool, tokenLedger, cfg.PoolRoyaltyShare,
		func() bool { return cfg.PoolRoyaltyMintingEnabled },
	)
	// 2.3b primitive #1: the per-pair rolling-window mint cap — bounds any
	// party's worst case to cap × s × avoided_COGS per pair per window.
	// 0 (default) disables; exact under concurrency via the after-CreditTx
	// count inside the existing mint tx (no new lock).
	royaltyMinter.SetCap(cfg.PoolMintCapPerPair, cfg.PoolMintCapWindow)
	// 2.3b per-ENTRY cap — bounds one entry_id's mints across all contributors
	// (ownership churn). 0 (default) disables. Shares the per-pair window.
	royaltyMinter.SetEntryCap(cfg.PoolMintCapPerEntry, cfg.PoolMintCapWindow)
	// 2.3a holdback: mints credit HELD; the sweeper below settles them after
	// this window. Trigger-agnostic — billing settlement can replace the
	// timed sweeper later without touching the mint path.
	royaltyMinter.SetHoldbackWindow(cfg.PoolHoldbackWindow)
	// U6 PR2: enable the owner-linkage wash guard in production — deny a pooled
	// royalty mint between two workspaces sharing a captured card fingerprint.
	// Default-allow-on-missing; the rate cap bounds yield regardless.
	royaltyMinter.SetOwnerLinkageCheck(true)
	p.SetRoyaltyMinter(royaltyMinter)

	// Stage-2.3a finalize sweeper: settles due held mints (held → spendable;
	// supply counts here via the TypePoolRoyalty row FinalizeHeldTx writes).
	// Mirrors the povi-challenge loop: leader-elected, minute tick.
	// Registered UNCONDITIONALLY — never gated on the minting flag: committed
	// held rows must finalize even if minting is later disabled, or
	// contributor LENS strands in held forever. With minting off and no held
	// rows, each sweep is a single cheap indexed SELECT.
	finalizeSweeper := poolroyalty.NewFinalizeSweeper(pool, tokenLedger, "pool_royalty_mints")
	// U3: economy worker — gated on the master switch so no held→final settlement
	// (an economy-state ledger write) runs when the economy is off.
	if cfg.EconomyEnabled {
		go haComps.leader.Run(ctx, "pool-royalty-finalize", 30*time.Second, func(lctx context.Context) {
			finalizeSweeper.StartScheduler(lctx, time.Minute)
		})
	}
	if cfg.PoolRoyaltyMintingEnabled {
		logger.Warn("poolroyalty: Pool-B royalty minting is ENABLED — served cross-tenant pooled hits mint LENS to contributors",
			slog.Float64("royalty_share", cfg.PoolRoyaltyShare))
	}

	// L2/S4 PR3: the distill reuse-royalty mint. A flag-gated, leader-elected
	// sweeper mints s × avoided_cogs_usd to the OCR contributor (owner A), ONCE
	// per cross-tenant reuse relationship, off the DEDUPLICATED distill_royalty_basis
	// table (PR2) — claim-then-act exactly-once, reusing the Pool-B held-ledger
	// kernel (CreditHeldTx → U6 floor on the contributor + per-identity rate cap +
	// TypePoolRoyaltyHeld). Inert by default: with LENS_POOL_ROYALTY_MINTING_ENABLED
	// =false the sweeper no-ops BEFORE any DB access, so basis rows accumulate but
	// nothing mints. The distill finalize sweeper is the SAME kernel parameterized
	// for the distill claim table; registered UNCONDITIONALLY (EconomyEnabled-gated
	// start) so held rows settle even if minting is later disabled.
	distillMinter := poolroyalty.NewDistillMinter(
		pool, tokenLedger, cfg.PoolRoyaltyShare,
		func() bool { return cfg.PoolRoyaltyMintingEnabled },
	)
	distillMinter.SetOwnerLinkageCheck(true) // U6 PR2 wash guard, like the cache minter
	distillMinter.SetHoldbackWindow(cfg.PoolHoldbackWindow)
	// PR1 distill mint caps (default 0/0 = off; deflationary — a cap only denies).
	distillMinter.SetCap(cfg.DistillMintCapPerPair, cfg.DistillMintCapWindow)
	distillMinter.SetContentCap(cfg.DistillMintCapPerContent, cfg.DistillMintCapWindow)
	distillFinalizeSweeper := poolroyalty.NewFinalizeSweeper(pool, tokenLedger, "distill_royalty_mints")
	if cfg.EconomyEnabled {
		go haComps.leader.Run(ctx, "distill-royalty-mint", 30*time.Second, func(lctx context.Context) {
			distillMinter.StartScheduler(lctx, time.Minute) // RunOnce no-ops while the mint flag is off
		})
		go haComps.leader.Run(ctx, "distill-royalty-finalize", 30*time.Second, func(lctx context.Context) {
			distillFinalizeSweeper.StartScheduler(lctx, time.Minute)
		})

		// The "smoke detector" — the scheduled cache+distill fraud-detector sweep. Mirrors
		// the finalize sweeper's leader.Run start. Economy-gated by this enclosing block (no
		// mints → nothing to scan); DetectorSweepEnabled defaults TRUE so detection accompanies
		// minting automatically (a manual off-switch only). READ-ONLY: it reads the mint tables
		// + records append-only findings + sets a gauge — it NEVER resolves/adjudicates or
		// touches a mint/balance/held row (the never-auto-act invariant; see detector_sweep.go).
		detectorSweep := poolroyalty.NewDetectorSweep(
			poolroyalty.NewDetectorReader(pool, thresholdsFromConfig(cfg)),
			poolroyalty.NewDistillDetectorReader(pool, thresholdsFromConfig(cfg)),
			poolroyalty.NewFindingsWriter(pool),
			cfg.DetectorSweepWindow,
		)
		go haComps.leader.Run(ctx, "royalty-detector-sweep", 30*time.Second, func(lctx context.Context) {
			if cfg.DetectorSweepEnabled {
				detectorSweep.StartScheduler(lctx, cfg.DetectorSweepInterval)
			}
		})
	}

	// Compute mining (Batch 2 Item 2). Wires its hook into the
	// localrouter so any verified GPU node that serves a
	// cross-workspace request gets credited automatically.
	computeMiner := mining.NewComputeMiner(tokenLedger, pool)
	computeMiner.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 5*time.Second))
	localRouterMulti.SetOnRequestServed(func(nodeID, requesterWS, requestID string, tokens int, latencyMs int64) {
		// RETIREMENT SWITCH (PoVI Part 3): the LEGACY trust-based mint (mints LENS
		// per served request, no receipt). U6: now DEFAULT OFF — an unprotected
		// mint path is opt-in. receipt-minting (LENS_POVI_MINTING_ENABLED) is the
		// intended successor. NOTE: NotifyServed has no caller today, so this hook
		// is dormant; when it is wired, requestID MUST be a server-derived
		// work-product hash (RecordServedRequest mints idempotently on it, and an
		// empty requestID mints nothing).
		if !cfg.TrustfulComputeMintEnabled {
			return
		}
		if err := computeMiner.RecordServedRequest(ctx, nodeID, requesterWS, requestID, tokens, latencyMs); err != nil {
			logger.Warn("compute mining: record served request failed",
				slog.String("node_id", nodeID), slog.String("err", err.Error()))
		}
	})

	// PoVI receipts (Token Economy Phase 1, Part 1). Verifies node-signed
	// receipts against each node's registered ed25519 pubkey, records them for
	// audit, and — ONLY when LENS_POVI_MINTING_ENABLED is on (default OFF,
	// and UNSAFE: a node can sign a fabricated trace) — performs a gated
	// provisional mint. Default behavior: verify + record, mint NOTHING.
	// NOTE: the existing ComputeMiner.RecordServedRequest above already mints
	// LENS on trust (no receipt); PoVI is designed to REPLACE it once Part 3
	// (challenge-and-slash) provides the economic security — that path stays
	// active and unsecured until then (tracked, not an oversight).
	poviStore := povi.NewStore(pool)
	poviLookup := func(lookupCtx context.Context, nodeID string) (ed25519.PublicKey, error) {
		enc, err := computeMiner.NodePubKey(lookupCtx, nodeID)
		if err != nil {
			return nil, err
		}
		return povi.DecodePublicKey(enc)
	}
	// PoVI staking (Part 2): node collateral that gates minting-eligibility and
	// that Part 3 will slash. Reuses the ledger's atomic lock primitive
	// (tokenLedger satisfies povi.StakeLedger) via the locked_balance column.
	poviStakeStore := povi.NewNodeStakeStore(pool)
	poviStakeManager := povi.NewStakeManager(
		poviStakeStore, tokenLedger, computeMiner.NodeWorkspace,
		cfg.POVIMinStake, cfg.POVIUnbondPeriod, pool)
	stakeEligible := func(eligCtx context.Context, nodeID string) bool {
		return poviStakeManager.IsEligible(eligCtx, nodeID)
	}
	// The mint gate is now verified AND stake-eligible AND minting-enabled
	// (still OFF by default).
	poviProcessor := povi.NewProcessor(poviStore, tokenLedger, poviLookup, stakeEligible, cfg.POVIMintingEnabled)
	if cfg.POVIMintingEnabled {
		logger.Warn("PoVI receipt minting is ENABLED (LENS_POVI_MINTING_ENABLED). Now SAFE only because Parts 1+2+3 are in place (verified receipt + staked node + challenge-and-slash). Ensure LENS_POVI_CHALLENGE_RATE > 0 so cheating is deterred.")
	}

	// PoVI challenge-and-slash (Part 3): the keystone. Lens randomly challenges
	// nodes to prove sampled Merkle paths; failures slash stake (Part 2),
	// making receipt-minting economically safe. Lens signs challenges with its
	// own ed25519 key (symmetric to Part 1's node-signed receipts); the node
	// verifies + answers from its retained trace.
	lensChallengePub, lensChallengePriv := loadOrGenChallengeKey(cfg.POVIChallengeKey, logger)
	challengeClient := povi.NewChallengeClient(lensChallengePriv, 10*time.Second)
	challengeClient.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 10*time.Second))
	challengeStore := povi.NewChallengeStore(pool)
	// Blocker 6: gateway auto-route to registered nodes. Wired here (not at proxy.New) because it
	// reuses the EXISTING challenge private key (lensChallengePriv, just loaded above) to sign the
	// per-request node-auth token. enabled=cfg.NodeAutoRouteEnabled (default false) → fully inert.
	p.SetNodeRouter(localRouterMulti, lensChallengePriv, newNodeHTTPClient(cfg.NodeTLSSkipVerify, 30*time.Second), cfg.NodeAutoRouteEnabled)
	poviChallenger := povi.NewChallenger(
		computeMiner.NodeURL, challengeClient, poviStakeManager, challengeStore,
		4, cfg.POVISlashFraction)
	challengeScheduler := povi.NewChallengeScheduler(poviChallenger, poviStore, cfg.POVIChallengeRate)
	// U3: economy worker — gated on the master switch (PoVI challenge/slash is an
	// economy-state mutation; no slashing when the economy is off).
	if cfg.EconomyEnabled {
		go haComps.leader.Run(ctx, "povi-challenge", 30*time.Second, func(lctx context.Context) {
			challengeScheduler.StartScheduler(lctx, time.Minute)
		})
	}

	// Embedding mining (Batch 2 Item 3). Shares the ledger;
	// proxy wires RecordEmbeddingsServed in the semantic-cache
	// path in a follow-up.
	embeddingMiner := mining.NewEmbeddingMiner(tokenLedger, pool)
	embeddingMiner.SetHTTPClient(newNodeHTTPClient(cfg.NodeTLSSkipVerify, 5*time.Second))
	_ = embeddingMiner

	// Annotation mining (Batch 2 Item 4). Proof-of-useful-work
	// — annotators stake LENS, review pairs of responses, and
	// earn per validated annotation.
	annotationMiner := mining.NewAnnotationMiner(tokenLedger, pool)

	// Annotation reputation SWEEP — ONE scheduled job that per tick (a) RESOLVES TTL-expired
	// tasks into agreement_outcome events from the FINAL consensus, and (b) DECAYS dormant
	// annotators' earned reputation toward baseline. Both are append-only writes to
	// reputation_events. Economy-gated + leader-elected, mirroring the detector sweep. Reputation
	// is MONEY-DECOUPLED: computed/gated/decayed/displayed but NEVER in the earning path
	// (annotation_mining.go SubmitAnnotation stays base + bonus) — pinned by an AST guard.
	reputationStore := mining.NewReputationStore(pool)
	if cfg.EconomyEnabled {
		go haComps.leader.Run(ctx, "annotation-reputation-resolution", 30*time.Second, func(lctx context.Context) {
			reputationStore.StartScheduler(lctx, time.Hour)
		})
	}

	// Pattern mining (Batch 2 Item 5). The deployment-level
	// PatternMiningEnabled flag ANDs with the per-workspace
	// opt-in before earnings fire (RecordPattern's optedIn arg).
	patternMiner := mining.NewPatternMiner(tokenLedger, pool)
	// S2 routing-pattern earn cap — overrides the miner's real default from
	// config. INERT this stage: RecordPattern (the earn path the cap guards) has
	// no production caller until S4, so the cap never runs live yet.
	patternMiner.SetEarnCap(cfg.PatternEarnCapPerWorkspace, cfg.PatternEarnCapWindow)

	// Routing intelligence (Upgrade 22). Consumes the opted-in pattern
	// aggregate to recommend best quality-per-dollar models. OFF by default;
	// when on, only auto-route requests are influenced. In-memory cohort
	// cache refreshed on a timer — the per-request path never hits the DB.
	// Cost basis reuses the alerts price table (blended $/1k tokens).
	routingAdvisor := routing.New(
		patternMiner,
		func(m string) float64 { return alerts.CostUSD(m, 500, 500) },
		routing.Config{Enabled: cfg.RoutingIntelligenceEnabled, TierCohorts: cfg.RoutingTierCohortsEnabled},
	)
	routingAdvisor.StartRefresh(ctx)
	p.SetRoutingAdvisor(routingAdvisor)

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

	// U18b billing (fiat Stripe → LXC credit). Constructed unconditionally so the
	// route handler method values are valid; the routes themselves register only
	// when cfg.BillingEnabled (billReg). Stripe keys are validated at config.Load
	// (startup fails if billing is enabled without them) and never logged.
	billingSvc := billing.New(pool, dualToken,
		billing.NewLiveStripe(cfg.StripeSecretKey, cfg.BillingSuccessURL, cfg.BillingCancelURL),
		cfg.StripeWebhookSecret)
	bill := billReg{on: cfg.BillingEnabled}
	// Stage 2.4/2.5 shadow LXC spend — observational, post-serve, flag-gated
	// (LENS_LXC_SHADOW_SPEND_ENABLED, default off). The proxy debits LXC
	// alongside the cost_usd write; void/non-gating, cannot affect serving.
	p.SetLXCSpendSink(dualToken, func() bool { return cfg.LXCShadowSpendEnabled })
	// LXC gating (Stage 2.4/2.5) — pre-serve block; inert unless LXCGatingEnabled
	// AND LXCShadowSpendEnabled are both on. Default off.
	p.SetLXCGate(dualToken, func() bool { return cfg.LXCGatingEnabled })
	// Phase-3 routing-pattern capture — post-serve, void, mint-free producer
	// for the routing Advisor. Default off; persists observations for opted-in
	// workspaces only (SQL gate). NEVER reaches ledger.Credit (earning is a
	// separate later stage).
	p.SetPatternCapture(patternMiner, func() bool { return cfg.PatternCaptureEnabled })
	p.SetObservationalLimiter(obsLimiter)
	// WorkTier descriptive classifier (default-off capability flag). Post-serve,
	// NON-CONTENT, mint-free; shares the obsLimiter budget. The ANALYTICS consumer
	// (GET /v1/admin/worktier/distribution, registered below) reads this same store via
	// Aggregate; the routing Advisor tier-conditioning is still a future PR.
	worktierStore := worktier.NewStore(pool)
	p.SetWorkTier(worktierStore, func() bool { return cfg.WorkTierEnabled })
	// S4 routing-pattern EARNING wire-up — the same miner, separate sink + flag.
	// Default off; flag-off the serve path is byte-identical to capture-only.
	p.SetPatternEarn(patternMiner, func() bool { return cfg.PatternEarningEnabled })

	r := chi.NewRouter()
	// U3 master economy kill-switch: the single chokepoint for economy-route
	// registration. econ.{get,post,del} register only when cfg.EconomyEnabled;
	// when off the routes are never mounted → chi-native 404 (#152). See
	// economy_routes.go.
	econ := econReg{on: cfg.EconomyEnabled}
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
	// Security headers on every response: X-Content-Type-Options, X-Frame-Options,
	// Referrer-Policy, and Content-Security-Policy (tuned for the dashboard's
	// Google Fonts dependency; all JS/CSS is inline).
	r.Use(api.SecurityHeadersMiddleware)
	// CORS: no-op when LENS_CORS_ALLOWED_ORIGINS is unset (default).
	// Set to a comma-separated list of origins or "*" for public-API mode.
	// Preflight OPTIONS requests are absorbed here, before auth middleware.
	r.Use(api.CORSMiddleware(cfg.CORSAllowedOrigins))
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
		// U8/U9: replica reachability + replay lag. Healthy no-op when no
		// replica is configured (the feature is off, not broken).
		"read_replica": replicaLagMonitor,
	})
	r.Get("/healthz", healthHandler.ServeHTTP)

	// HA endpoints (Upgrade 7). The existing /healthz above is intentionally
	// left unchanged for backward-compat; these are additive:
	//   /livez     — pure liveness, always 200 while the process serves
	//   /readyz    — drain-aware readiness; 503 while draining or a dep is down
	//   /ha/status — cluster view (this instance + peers) for ops + dashboard
	r.Get("/livez", haComps.health.Live)
	r.Get("/readyz", haComps.health.Ready)
	// /ha/status exposes cluster peer list + instance health — admin-only
	// (ISO 27001 A.9). /livez and /readyz stay public for load balancers.
	r.Get("/ha/status", requireAdmin(authManager, http.HandlerFunc(haComps.health.Status)))

	// OpenAPI spec — public, no auth, no version path prefix.
	r.Get("/openapi.json", api.ServeOpenAPI)

	// Public read-only API endpoints — no auth required, but rate-limited to
	// prevent enumeration and DoS on the public economic surface.
	r.Group(func(pub chi.Router) {
		pub.Use(ratelimit.RateLimitMiddleware(rateLimiter))

		// Token earning rates — part of Lens's public economic surface.
		econ.get(pub, "/v1/tokens/rates", func(w http.ResponseWriter, _ *http.Request) {
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

		// Economy stats (Batch 3 Phase 1).
		econ.get(pub, "/v1/economy/stats", func(w http.ResponseWriter, req *http.Request) {
			stats, err := marketplace.GetEconomyStats(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// Marketplace listings (read-only — browsing is public; buying requires auth).
		econ.get(pub, "/v1/marketplace/listings", func(w http.ResponseWriter, req *http.Request) {
			limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
			listings, err := marketplace.GetListings(req.Context(), limit)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, listings)
		})

		// Aggregated routing patterns across opted-in workspaces.
		econ.get(pub, "/v1/insights/routing", func(w http.ResponseWriter, req *http.Request) {
			q := req.URL.Query()
			insights, err := patternMiner.GetInsights(req.Context(),
				q.Get("model"), q.Get("provider"), q.Get("feature"))
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, insights)
		})

		// Discovery: embedding nodes available for a model.
		pub.Get("/v1/embedding-nodes/available", func(w http.ResponseWriter, req *http.Request) {
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

		// Discovery: GPU nodes available for a model.
		pub.Get("/v1/nodes/available", func(w http.ResponseWriter, req *http.Request) {
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

		// JWKS — public EC P-256 verification key for the ES256 JWTs
		// Lens mints. External services fetch this once (or on cache miss)
		// to verify tokens without holding the private key.
		// Standard path per RFC 8414 discovery conventions.
		pub.Get("/v1/auth/jwks", func(w http.ResponseWriter, _ *http.Request) {
			pub := authManager.PublicKey()
			if pub == nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "JWT signing not configured")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=3600")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []any{auth.PublicKeyToJWK(pub, auth.JWTKid)},
			})
		})
	})

	// /metrics exposes Prometheus telemetry — gated to admin key so internal
	// server stats are not visible to unauthenticated callers (ISO 27001 A.9).
	// Prometheus scrapers must send: Authorization: Bearer <LENS_API_KEY>.
	r.Handle("/metrics", requireAdmin(authManager, metrics.Handler()))

	apiServer := api.NewServer(
		pool, redisClient, nc, exactCache, l,
		alertManager, branchTracker, wsManager, lr,
		anomalyDetector,
		"0.1.0",
	)
	// Budgets store powers the dashboard's read-only Budgets panel.
	apiServer.SetBudgetStore(budgetStore)
	// Forecaster powers the dashboard's projection columns.
	apiServer.SetForecaster(forecaster)
	// Cost-anomaly detector powers the dashboard's Cost outliers panel.
	apiServer.SetCostAnomalyDetector(costAnomalyDetector)
	// ROI reporter powers the dashboard's Executive summary panel.
	apiServer.SetROIReporter(roiReporter)
	// Routing advisor powers the dashboard's Routing intelligence panel.
	apiServer.SetRoutingAdvisor(routingAdvisor)
	// Guardrails engine powers the dashboard's Guardrails panel.
	apiServer.SetGuardrailsEngine(guardrailsEngine)
	apiServer.SetEvalPipeline(evalPipeline)
	apiServer.SetPOVIStakeManager(poviStakeManager)
	apiServer.SetPOVIChallengeStore(challengeStore)
	apiServer.SetEconomyEnabled(cfg.EconomyEnabled) // U3: gate the economy dashboard-API routes
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
	dashHandler := dashboard.New("0.1.0", cfg.EconomyEnabled)
	r.Get("/dashboard", dashHandler.ServeHTTP)
	econ.get(r, "/dashboard/tokens", dashHandler.ServeTokens)
	r.Get("/dashboard/nodes", dashHandler.ServeNodes)
	econ.get(r, "/dashboard/oracle", dashHandler.ServeOracle)
	econ.get(r, "/dashboard/economy", dashHandler.ServeEconomy)
	r.Get("/", dashHandler.RedirectRoot)

	// Public oracle + economy endpoints — no auth, no PII, but rate-limited
	// to prevent scraping / DoS.
	r.Group(func(pub chi.Router) {
		pub.Use(ratelimit.RateLimitMiddleware(rateLimiter))

		econ.get(pub, "/v1/oracle/stats", func(w http.ResponseWriter, req *http.Request) {
			stats, err := oracleEngine.GetOracleStats(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// Public conversion-rate surface (two-token split). The rate +
		// its full derivation are public economic signal; no auth.
		econ.get(pub, "/v1/economy/conversion-rate", func(w http.ResponseWriter, req *http.Request) {
			rate, err := rateEngine.CurrentRate(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{
				"rate":         rate,
				"usd_per_lxc":  economy.LXCUSDValue,
				"lens_per_lxc": rate,
			})
		})

		econ.get(pub, "/v1/economy/conversion-rate/history", func(w http.ResponseWriter, req *http.Request) {
			limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
			hist, err := rateEngine.RateHistory(req.Context(), limit)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, hist)
		})
	})

	// Public status page. /status content-negotiates between HTML and
	// JSON; /status.json is the unconditional-JSON convenience route
	// uptime monitors and CI scripts use.
	r.Get("/status", statusPage.ServeHTTP)
	r.Get("/status.json", statusPage.ServeJSON)

	// Everything else sits behind the API-key middleware. chi.Group inherits
	// middleware only for routes registered inside its closure.
	heliconeCompat := compat.NewHeliconeCompat(keyStore)

	// U18b billing webhook — PUBLIC (Stripe-signed, no auth), gated on
	// BillingEnabled (unregistered ⇒ 404). Registered on the bare router with no
	// auth/rate-limit middleware; the handler reads the RAW body itself (Stripe
	// signs raw bytes) before any JSON.
	bill.post(r, "/v1/billing/webhook", billingSvc.HandleWebhook)

	r.Group(func(authed chi.Router) {
		// Helicone-compat translates Helicone-* headers and rewrites
		// /oai/* and /anthropic/* paths BEFORE auth runs. That order
		// matters: Helicone-Auth becomes Authorization, which is what
		// AuthMiddleware then validates.
		authed.Use(heliconeCompat.Middleware())
		// Auth must run before the rate-limiter so the limiter sees the
		// key/workspace that AuthMiddleware just stamped onto the request.
		authed.Use(auth.AuthMiddleware(keyStore, authManager))
		authed.Use(ratelimit.RateLimitMiddleware(rateLimiter))
		// Tenant isolation: a /v1/workspaces/{wsID}/... route may only be driven
		// by a credential belonging to that workspace (the global admin key
		// bypasses). Registered after AuthMiddleware so the resolved identity is
		// already on the request context. Closes a cross-tenant BOLA/IDOR where
		// any valid workspace key could act on any {wsID} in the path — read or
		// rewrite another tenant's config/budgets and mint API keys for it.
		authed.Use(workspaceIsolationMiddleware)

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

		authed.Post("/v1/api/keys", newCreateAPIKeyHandler(keyStore))

		authed.Post("/v1/api/injection/patterns", requireAdmin(authManager, http.HandlerFunc(newInjectionPatternAddHandler(injectionDetector))))

		authed.Delete("/v1/api/keys/{keyID}", newRevokeAPIKeyHandler(keyStore))

		// ─── JWT auth endpoints ────────────────────────
		// /v1/auth/token mints a JWT — admin-only because issuing
		// a token for an arbitrary workspace/user is a privileged
		// op. /refresh accepts any valid JWT and extends it.
		// /me echoes the resolved AuthContext for the caller.
		authed.Post("/v1/auth/token", func(w http.ResponseWriter, req *http.Request) {
			if authManager.PrivateKey() == nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "JWT signing not available")
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
			tok, err := auth.GenerateToken(in.WorkspaceID, in.UserID, in.Scopes, authManager.PrivateKey(), ttl)
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
		econ.post(authed, "/v1/admin/conversion-rate/approve", func(w http.ResponseWriter, req *http.Request) {
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

		// Admin-only DISTILL preview: a DRY RUN that converts an uploaded
		// document to Markdown through the KILLABLE subprocess (ProcessIsolator,
		// using the proven-sufficient 512 MiB default) — never the in-process
		// path. No model call, no token_events, no spend. A text-less/scanned
		// doc returns needs_vision honestly (vision OCR is a later PR).
		distillPreview := &distillpreview.Handler{
			Converter: &distill.ProcessIsolator{WorkerBin: cfg.DistillWorkerBin},
			IsAdmin: func(req *http.Request) bool {
				// Mirror the existing admin route's error-first, fail-closed check
				// (deny on auth error OR non-admin) exactly.
				actx, err := authManager.Authenticate(req)
				if err != nil || !actx.IsAdmin {
					return false
				}
				return true
			},
		}
		authed.Post("/v1/admin/distill/preview", distillPreview.ServeHTTP)

		// ADMIN-ONLY distill attribution read (S1 read-surface commitment).
		// requireAdmin-gated: content_hash + counterparty workspace ids are
		// exposed here and must never be tenant-reachable. Default returns raw
		// rows; ?view=pairs returns the condition-(b) materiality aggregate. The
		// Reader is Query-only (no write capability — see distillattrib.Reader).
		econ.get(authed, "/v1/admin/distill/attribution",
			requireAdmin(authManager, http.HandlerFunc(newDistillAttributionAdminHandler(distillattrib.NewReader(dbrouting.ReadPool(pool, replicaPool))))))

		// Stage-3 pool-mint adjudication gate — the Revoker's FIRST and ONLY
		// production caller. Admin-gated (mirrors ApproveRate); the operator
		// passes an explicitly-chosen subset of held request_ids to revoke. The
		// writer records the decision BEFORE the burn (record-before-revoke), so
		// no production revoke can happen without a preceding audit row. Doubly
		// inert in the current config: needs an admin AND
		// LENS_POOL_ROYALTY_MINTING_ENABLED (no held rows exist otherwise). Never
		// a loop — operator-initiated only.
		royaltyRevoker := poolroyalty.NewRevoker(pool, tokenLedger)
		royaltyAdjudicator := poolroyalty.NewAdjudicationWriter(pool, royaltyRevoker)
		econ.post(authed, "/v1/admin/pool-royalty/adjudicate", newAdjudicateHandler(authManager, royaltyAdjudicator))

		// PR3 distill anti-gaming — the SAME record-before-revoke gate over the
		// distill_royalty_mints / distill_royalty_adjudications tables (the Revoker +
		// AdjudicationWriter parameterized by table; RevokeHeldTx is already generic).
		// Same admin gate, same inertness (held distill rows exist only under the mint flag).
		distillRoyaltyRevoker := poolroyalty.NewRevokerForTable(pool, tokenLedger, "distill_royalty_mints")
		distillRoyaltyAdjudicator := poolroyalty.NewAdjudicationWriterForTable(pool, distillRoyaltyRevoker, "distill_royalty_adjudications")
		econ.post(authed, "/v1/admin/distill-royalty/adjudicate", newAdjudicateHandler(authManager, distillRoyaltyAdjudicator))

		// Pool-B royalty OBSERVABILITY (read-only, admin-gated, NOT economy-gated).
		// Forensic infra must survive the kill-switch — the economy is likeliest OFF
		// during a security event, exactly when detection is needed. The readers use
		// Query-only db seams (no Exec/Begin reachable): NO mint/burn/balance/held row
		// is touched, and the adjudicate/clawback path above is untouched. requireAdmin
		// → 401 (the existing admin-reader gate); registered on `authed` directly (NOT
		// econ.get), so they resolve regardless of LENS_ECONOMY_ENABLED. Only the
		// MUTATION endpoint (adjudicate, above) stays economy-gated.
		poolRoyaltyDetector := poolroyalty.NewDetectorReader(pool, thresholdsFromConfig(cfg))
		poolRoyaltyResolver := poolroyalty.NewResolver(pool)
		poolRoyaltyMargin := poolroyalty.NewMarginReader(pool)
		authed.Get("/v1/admin/pool-royalty/detect", requireAdmin(authManager, http.HandlerFunc(newPoolRoyaltyDetectHandler(poolRoyaltyDetector))))
		authed.Get("/v1/admin/pool-royalty/resolve", requireAdmin(authManager, http.HandlerFunc(newPoolRoyaltyResolveHandler(poolRoyaltyResolver))))
		authed.Get("/v1/admin/pool-royalty/margin", requireAdmin(authManager, http.HandlerFunc(newPoolRoyaltyMarginHandler(poolRoyaltyMargin))))

		// WorkTier ANALYTICS (read-only, admin-gated, NOT economy-gated) — surfaces ONE
		// workspace's descriptive tier distribution from worktier.Store.Aggregate (Query-only;
		// no Exec/Begin reachable, no mint path). workspace_id is REQUIRED (no cross-tenant
		// mode). Registered on `authed` (NOT econ.get) to match the WorkTier capture's
		// kill-switch-EXEMPT posture — the read survives LENS_ECONOMY_ENABLED, like the capture
		// it surfaces. MONEY-DECOUPLED: WorkTier never feeds mint/earn/billing.
		authed.Get("/v1/admin/worktier/distribution", requireAdmin(authManager, http.HandlerFunc(newWorkTierDistributionHandler(worktierStore))))

		// Annotation reputation ADMIN RE-ENTRY (PR2) — un-bench a sub-floor annotator by appending
		// an admin_reset event (append-only; restores to baseline). Admin-gated (requireAdmin →
		// 401), NOT economy-gated (an operator action, like the observability reads). MONEY-DECOUPLED:
		// it moves a reputation score, never the ledger. reputationStore is the same one the
		// resolution job uses.
		authed.Post("/v1/admin/annotation-reputation/reset", requireAdmin(authManager, http.HandlerFunc(newReputationResetHandler(authManager, reputationStore))))

		// Distill royalty OBSERVABILITY — the distill mirror of the Pool-B block above,
		// over distill_royalty_mints / distill_royalty_margin. Same read-only admin-gated
		// (NOT economy-gated) shape; NO similarity detector + NO resolver (distill has
		// neither, by design). Read-only seams; the distill adjudicate/clawback path + any
		// sweeper are untouched.
		distillRoyaltyDetector := poolroyalty.NewDistillDetectorReader(pool, thresholdsFromConfig(cfg))
		distillRoyaltyMargin := poolroyalty.NewDistillMarginReader(pool)
		distillRoyaltyResolver := poolroyalty.NewDistillResolver(pool)
		authed.Get("/v1/admin/distill-royalty/detect", requireAdmin(authManager, http.HandlerFunc(newDistillRoyaltyDetectHandler(distillRoyaltyDetector))))
		authed.Get("/v1/admin/distill-royalty/resolve", requireAdmin(authManager, http.HandlerFunc(newDistillRoyaltyResolveHandler(distillRoyaltyResolver))))
		authed.Get("/v1/admin/distill-royalty/margin", requireAdmin(authManager, http.HandlerFunc(newDistillRoyaltyMarginHandler(distillRoyaltyMargin))))

		authed.Post("/v1/auth/refresh", func(w http.ResponseWriter, req *http.Request) {
			if authManager.PrivateKey() == nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "JWT signing not available")
				return
			}
			actx, err := authManager.Authenticate(req)
			if err != nil || actx.AuthMethod != auth.MethodJWT {
				writeJSONErr(w, http.StatusUnauthorized, "valid JWT required")
				return
			}
			ttl := auth.ClampTTL(cfg.TokenTTL)
			tok, err := auth.GenerateToken(actx.WorkspaceID, actx.UserID, actx.Scopes, authManager.PrivateKey(), ttl)
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
			// RotateAPIKey is a single atomic transaction: SELECT FOR UPDATE on the
			// old key → INSERT new key → DELETE old key. Concurrent rotate calls for
			// the same keyID are serialised by the row-level lock; the second call
			// finds the row gone and gets a 404. This prevents the previous race
			// where two concurrent calls could produce two simultaneously active keys.
			raw, fresh, err := tenantStore.RotateAPIKey(req.Context(), wsID, keyID)
			if errors.Is(err, tenant.ErrKeyNotFound) {
				writeJSONErr(w, http.StatusNotFound, "key not found")
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			logger.Info("auth: key rotated",
				slog.String("workspace_id", wsID),
				slog.String("old_key_id", keyID),
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
			wsID := chi.URLParam(req, "wsID")
			keyID := chi.URLParam(req, "keyID")
			// workspaceIsolationMiddleware has already proven the caller owns
			// {wsID}. RevokeAPIKey operates by keyID alone, so confirm the key
			// actually belongs to {wsID} before revoking — otherwise a caller
			// could revoke another tenant's key by passing its ID under their
			// own workspace path.
			keys, err := tenantStore.ListAPIKeys(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			owned := false
			for _, k := range keys {
				if k.ID == keyID {
					owned = true
					break
				}
			}
			if !owned {
				writeJSONErr(w, http.StatusNotFound, "key not found")
				return
			}
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

		// ─── executive ROI reporting (Upgrade 24) ───
		// Read-only, cached. format=json (default) | html | md. The HTML is
		// self-contained + print-to-PDF-able; emailing it is Track's job.
		authed.Get("/v1/workspaces/{wsID}/roi/report", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			q := req.URL.Query()
			rep, err := roiReporter.GenerateReport(req.Context(), wsID, q.Get("period"))
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			switch q.Get("format") {
			case "html":
				html, err := roi.RenderHTML(rep)
				if err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(html))
			case "md", "markdown":
				w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
				_, _ = w.Write([]byte(roi.RenderMarkdown(rep)))
			default:
				writeJSONOK(w, http.StatusOK, rep)
			}
		})

		authed.Get("/v1/workspaces/{wsID}/roi/report/summary", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			summary, err := roiReporter.GenerateSummary(req.Context(), wsID, req.URL.Query().Get("period"))
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, summary)
		})

		// ─── routing intelligence introspection (Upgrade 22) ───
		// Read-only: lets users SEE what the advisor would pick (basis +
		// numbers) so the intelligence isn't a black box.
		authed.Get("/v1/workspaces/{wsID}/routing/recommendation", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			q := req.URL.Query()
			inputRange := q.Get("input_range")
			if inputRange == "" {
				inputRange = mining.InputBucketMedium
			}
			provider := q.Get("provider")
			if provider == "" {
				provider = "openai"
			}
			var allowedModels, allowedProviders []string
			if ws, ok := wsManager.GetWorkspace(wsID); ok {
				allowedModels, allowedProviders = ws.AllowedModels, ws.AllowedProviders
			}
			rec := routingAdvisor.RecommendByRange(req.Context(), wsID, q.Get("feature"), inputRange, provider, allowedModels, allowedProviders)
			writeJSONOK(w, http.StatusOK, rec)
		})

		authed.Get("/v1/routing/intelligence/status", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, map[string]any{
				"status":  routingAdvisor.Status(),
				"cohorts": routingAdvisor.Overview(),
			})
		})

		// ─── model capabilities (Upgrade 15) ───
		// Which models support which modalities — so capability-aware
		// routing isn't a black box.
		authed.Get("/v1/models/capabilities", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, modality.CapabilityMap())
		})

		// ─── model catalog (Upgrade 16) ───
		// The single source of truth for models — provider, pricing,
		// capabilities, context. Read-only introspection so pricing/caps are
		// transparent, not hidden.
		authed.Get("/v1/catalog/models", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, catalog.All())
		})
		authed.Get("/v1/catalog/models/{id}", func(w http.ResponseWriter, req *http.Request) {
			m, ok := catalog.Get(chi.URLParam(req, "id"))
			if !ok {
				writeJSONErr(w, http.StatusNotFound, "model not in catalog")
				return
			}
			writeJSONOK(w, http.StatusOK, m)
		})

		// ─── Local endpoint registry ───────────────────
		// Admin surface for the multi-endpoint Router. Listing
		// is read-only; the other three routes mutate the
		// in-process registry (no persistence — restarts re-read
		// LENS_LOCAL_ENDPOINTS).
		authed.Get("/v1/local/endpoints", func(w http.ResponseWriter, _ *http.Request) {
			writeJSONOK(w, http.StatusOK, localRouterMulti.List())
		})

		authed.Post("/v1/local/endpoints", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
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
		})))

		authed.Delete("/v1/local/endpoints/{id}", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			if !localRouterMulti.Remove(id) {
				writeJSONErr(w, http.StatusNotFound, "endpoint not found")
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})))

		authed.Post("/v1/local/endpoints/{id}/check", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			ep, err := localRouterMulti.CheckHealthByID(req.Context(), id)
			if err != nil {
				writeJSONErr(w, http.StatusNotFound, "endpoint not found")
				return
			}
			writeJSONOK(w, http.StatusOK, ep)
		})))

		// ─── LENS token endpoints (Batch 2 Item 1) ─────
		econ.get(authed, "/v1/workspaces/{wsID}/tokens/balance", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			snap, err := tokenLedger.GetSnapshot(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, snap)
		})

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/history", func(w http.ResponseWriter, req *http.Request) {
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
		// U18: LXC is the fiat usage credit — the balance READ is fiat (registers
		// unconditionally; survives LENS_ECONOMY_ENABLED=false). The convert route
		// below STAYS economy-gated (it burns LENS).
		authed.Get("/v1/workspaces/{wsID}/lxc/balance", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			snap, err := dualToken.GetLXCSnapshot(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, snap)
		})

		// U18b billing — FIAT, registered under BillingEnabled (independent of the
		// economy master). Checkout is workspace-scoped: workspaceIsolationMiddleware
		// binds {wsID} to the caller's credential (admin bypasses), like its siblings.
		bill.post(authed, "/v1/workspaces/{wsID}/billing/checkout", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				USDCents int64 `json:"usd_cents"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			url, err := billingSvc.CreateCheckout(req.Context(), wsID, in.USDCents)
			if err != nil {
				status := http.StatusInternalServerError
				if errors.Is(err, billing.ErrAmountNotAllowed) {
					status = http.StatusBadRequest
				}
				writeJSONErr(w, status, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]string{"url": url})
		})

		// Admin refund-visibility list (read-only; requireAdmin). An 'anomalous' row
		// means the customer was CHARGED and NOT credited — v1 resolution is a manual
		// refund in the Stripe dashboard.
		bill.get(authed, "/v1/admin/billing/purchases",
			requireAdmin(authManager, newBillingPurchasesHandler(billingSvc)))

		econ.post(authed, "/v1/workspaces/{wsID}/lxc/convert", func(w http.ResponseWriter, req *http.Request) {
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

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/mining/cache", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := cacheMiner.GetMiningStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/mining/compute", func(w http.ResponseWriter, req *http.Request) {
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

		// Node heartbeat (Batch 3 Phase 2). Workspace-scoped UPDATE:
		// the SQL requires id AND workspace_id so a key from workspace-A
		// cannot keep alive workspace-B's nodes (cross-workspace hijack fix).
		authed.Post("/v1/workspaces/{wsID}/nodes/{nodeID}/heartbeat", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodeID := chi.URLParam(req, "nodeID")
			var in struct {
				ActiveRequests int64    `json:"active_requests"`
				UptimeSeconds  int64    `json:"uptime_seconds"`
				ModelsLoaded   []string `json:"models_loaded"`
			}
			_ = json.NewDecoder(req.Body).Decode(&in)
			// Buffer heartbeat in Redis — any instance contributes to the shared
			// liveness view; the Reconciler reads Redis freshness when building
			// NodeSnapshots (xDS HA heartbeat reuse).
			_ = cpHB.Record(req.Context(), "inference", nodeID, in.UptimeSeconds)
			if pool != nil {
				tag, err := pool.Exec(req.Context(), `
					UPDATE inference_nodes
					SET last_seen_at = NOW(), uptime_seconds = $2
					WHERE id = $1 AND workspace_id = $3`, nodeID, in.UptimeSeconds, wsID)
				if err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				if tag.RowsAffected() == 0 {
					writeJSONErr(w, http.StatusNotFound, "node not found")
					return
				}
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		authed.Delete("/v1/workspaces/{wsID}/nodes/{nodeID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodeID := chi.URLParam(req, "nodeID")
			if err := computeMiner.DeactivateNode(req.Context(), nodeID, wsID); err != nil {
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
			secretHash, err := bcrypt.GenerateFromPassword([]byte(in.NodeSecret), bcrypt.DefaultCost)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, "hash node secret: "+err.Error())
				return
			}
			// Both INSERTs run in one transaction: if the metrics
			// seed fails the node row rolls back so heartbeat
			// UPDATEs never silently no-op against a metrics-less node.
			tx, err := pool.Begin(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			var id string
			var createdAt time.Time
			row := tx.QueryRow(req.Context(), `
				INSERT INTO cache_nodes (workspace_id, url, max_size_gb, node_secret_hash)
				VALUES ($1, $2, $3, $4)
				RETURNING id, created_at`,
				wsID, strings.TrimRight(in.URL, "/"), in.MaxSizeGB, string(secretHash))
			if err := row.Scan(&id, &createdAt); err != nil {
				_ = tx.Rollback(req.Context())
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			// Seed metrics row so heartbeat UPDATEs have a target.
			if _, err := tx.Exec(req.Context(),
				`INSERT INTO cache_node_metrics (node_id) VALUES ($1) ON CONFLICT (node_id) DO NOTHING`, id); err != nil {
				_ = tx.Rollback(req.Context())
				writeJSONErr(w, http.StatusInternalServerError, "seed metrics: "+err.Error())
				return
			}
			if err := tx.Commit(req.Context()); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, map[string]any{
				"id":           id,
				"workspace_id": wsID,
				"url":          in.URL,
				"max_size_gb":  in.MaxSizeGB,
				"created_at":   createdAt,
			})
		})

		authed.Delete("/v1/workspaces/{wsID}/cache-nodes/{nodeID}", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodeID := chi.URLParam(req, "nodeID")
			if pool == nil {
				writeJSONErr(w, http.StatusServiceUnavailable, "DB unavailable")
				return
			}
			// workspace_id scope: prevents cross-workspace node deletion.
			tag, err := pool.Exec(req.Context(),
				`UPDATE cache_nodes SET active = FALSE WHERE id = $1 AND workspace_id = $2`, nodeID, wsID)
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
			wsID := chi.URLParam(req, "wsID")
			nodeID := chi.URLParam(req, "nodeID")
			var in struct {
				Entries int     `json:"entries"`
				SizeMB  float64 `json:"size_mb"`
				HitRate float64 `json:"hit_rate"`
			}
			_ = json.NewDecoder(req.Body).Decode(&in)
			// Buffer heartbeat in Redis for xDS HA heartbeat reuse.
			_ = cpHB.Record(req.Context(), "cache", nodeID, 0)
			if pool != nil {
				// last_seen_at and metrics must move together atomically;
				// a partial update leaves the node appearing alive with
				// stale metrics or updated metrics with a stale timestamp.
				htx, err := pool.Begin(req.Context())
				if err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				// workspace_id scope: prevents cross-workspace heartbeat hijack.
				tag, err := htx.Exec(req.Context(), `
					UPDATE cache_nodes SET last_seen_at = NOW() WHERE id = $1 AND workspace_id = $2`, nodeID, wsID)
				if err != nil {
					_ = htx.Rollback(req.Context())
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				if tag.RowsAffected() == 0 {
					_ = htx.Rollback(req.Context())
					writeJSONErr(w, http.StatusNotFound, "node not found")
					return
				}
				if _, err := htx.Exec(req.Context(), `
					INSERT INTO cache_node_metrics (node_id, entries, size_mb, hit_rate, last_updated)
					VALUES ($1, $2, $3, $4, NOW())
					ON CONFLICT (node_id) DO UPDATE
					SET entries = EXCLUDED.entries,
					    size_mb = EXCLUDED.size_mb,
					    hit_rate = EXCLUDED.hit_rate,
					    last_updated = NOW()`,
					nodeID, in.Entries, in.SizeMB, in.HitRate); err != nil {
					_ = htx.Rollback(req.Context())
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				if err := htx.Commit(req.Context()); err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// ─── Embedding node CRUD (embedding mining) ────
		authed.Post("/v1/workspaces/{wsID}/embedding-nodes", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			// Decode into an inline struct so we can capture node_secret
			// without exposing it on mining.EmbeddingNode's JSON shape.
			var body struct {
				mining.EmbeddingNode
				NodeSecret string `json:"node_secret"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in := body.EmbeddingNode
			in.WorkspaceID = wsID
			// Hash the node secret and store the hash — mirrors cache-node
			// registration. An empty secret is allowed for backwards
			// compatibility with nodes that pre-date the secret field;
			// NULLIF in the INSERT stores NULL for an empty hash.
			if body.NodeSecret != "" {
				hash, err := bcrypt.GenerateFromPassword([]byte(body.NodeSecret), bcrypt.DefaultCost)
				if err != nil {
					writeJSONErr(w, http.StatusInternalServerError, "hash node secret: "+err.Error())
					return
				}
				in.NodeSecretHash = string(hash)
			}
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
			wsID := chi.URLParam(req, "wsID")
			nodeID := chi.URLParam(req, "nodeID")
			if err := embeddingMiner.DeactivateEmbeddingNode(req.Context(), nodeID, wsID); err != nil {
				if errors.Is(err, mining.ErrNodeNotFound) {
					writeJSONErr(w, http.StatusNotFound, "node not found")
					return
				}
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		// Embedding-node heartbeat — mirrors the inference-node and cache-node
		// heartbeat endpoints.  Refreshes last_seen_at via the control-plane
		// store so the reconciler can track liveness uniformly across all node
		// types (migration 0035 added last_seen_at to embedding_nodes).
		authed.Post("/v1/workspaces/{wsID}/embedding-nodes/{nodeID}/heartbeat", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			nodeID := chi.URLParam(req, "nodeID")
			var in struct {
				SpeedTPS      int64 `json:"speed_tps"`
				Inflight      int64 `json:"inflight"`
				UptimeSeconds int64 `json:"uptime_seconds"`
			}
			_ = json.NewDecoder(req.Body).Decode(&in)
			// Buffer heartbeat in Redis for xDS HA heartbeat reuse.
			_ = cpHB.Record(req.Context(), "embedding", nodeID, in.UptimeSeconds)
			// workspace_id scope: prevents cross-workspace heartbeat hijack.
			found, err := cpStore.RecordEmbedHeartbeat(req.Context(), nodeID, wsID, in.UptimeSeconds)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if !found {
				writeJSONErr(w, http.StatusNotFound, "node not found")
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/mining/embeddings", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			stats, err := embeddingMiner.GetStats(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, stats)
		})

		// ─── Annotation mining (Batch 2 Item 4) ─────────
		econ.post(authed, "/v1/workspaces/{wsID}/annotate/stake", func(w http.ResponseWriter, req *http.Request) {
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

		econ.del(authed, "/v1/workspaces/{wsID}/annotate/stake", func(w http.ResponseWriter, req *http.Request) {
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

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/mining/annotations", func(w http.ResponseWriter, req *http.Request) {
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
		econ.post(authed, "/v1/workspaces/{wsID}/pattern-mining/opt-in", func(w http.ResponseWriter, req *http.Request) {
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

		econ.del(authed, "/v1/workspaces/{wsID}/pattern-mining/opt-in", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			if err := patternMiner.OptOut(req.Context(), wsID); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"opted_in": false})
		})

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/mining/patterns", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			c, err := patternMiner.GetContribution(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, c)
		})

		// ─── Token transfers + marketplace + staking ────
		econ.post(authed, "/v1/workspaces/{wsID}/tokens/transfer", func(w http.ResponseWriter, req *http.Request) {
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

		econ.post(authed, "/v1/marketplace/listings", func(w http.ResponseWriter, req *http.Request) {
			var in economy.MarketplaceListing
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			// Authz (#146): a non-admin lists only as ITSELF (the seller is the
			// caller); admin may list on behalf of any seller via the body.
			eff, _, ok := effectiveWorkspaceID(req, in.SellerID)
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
			in.SellerID = eff
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

		econ.post(authed, "/v1/marketplace/listings/{id}/buy", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			var in struct {
				BuyerWorkspace string  `json:"buyer_workspace"`
				AmountUSD      float64 `json:"amount_usd"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			// Authz (#146): the buyer is the CALLER for non-admins; admin honors the body.
			eff, _, ok := effectiveWorkspaceID(req, in.BuyerWorkspace)
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
			in.BuyerWorkspace = eff
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

		econ.del(authed, "/v1/marketplace/listings/{id}", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			// Authz (#146): a non-admin may cancel only its OWN listing; admin
			// may act on any seller via the param.
			wsID, _, ok := effectiveWorkspaceID(req, req.URL.Query().Get("workspace_id"))
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
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

		econ.get(authed, "/v1/marketplace/trades", newMarketplaceTradesHandler(marketplace))

		econ.post(authed, "/v1/workspaces/{wsID}/tokens/stake", func(w http.ResponseWriter, req *http.Request) {
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

		econ.post(authed, "/v1/workspaces/{wsID}/tokens/stake/{positionID}/unstake", func(w http.ResponseWriter, req *http.Request) {
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

		econ.get(authed, "/v1/workspaces/{wsID}/tokens/stakes", func(w http.ResponseWriter, req *http.Request) {
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
		// (migration 0017) — now the SOLE attribution source (the
		// branch_spend double-write was retired in #157).
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

		// Per-workspace DISTILL policy toggle (stage 3, PR #2). Applies
		// immediately — proxy.serve() reads via GetDistillPolicy on every
		// request. Default is disabled, so the request path stays inert until an
		// admin enables a workspace here.
		authed.Put("/v1/workspaces/{wsID}/distill", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				DistillPolicy workspace.DistillPolicy `json:"distill_policy"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.SetDistillPolicy(req.Context(), wsID, in.DistillPolicy); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ws, _ := wsManager.GetWorkspace(wsID)
			writeJSONOK(w, http.StatusOK, map[string]any{
				"ok":             true,
				"distill_policy": ws.DistillPolicy,
			})
		})

		// Phase-2 Stage 2.0 shared-cache governance gate (exact cache): per-tenant
		// opt-in for cross-user cache pooling. Default false (private), so the
		// request path stays inert until an admin opts a workspace in here AND the
		// global LENS_CACHE_POOLABLE_ENABLED switch is on. This route sits behind
		// the same auth + #84 workspace-isolation middleware as the /distill route.
		authed.Put("/v1/workspaces/{wsID}/cache-poolable", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				CachePoolable bool `json:"cache_poolable"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.SetCachePoolable(req.Context(), wsID, in.CachePoolable); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ws, _ := wsManager.GetWorkspace(wsID)
			writeJSONOK(w, http.StatusOK, map[string]any{
				"ok":             true,
				"cache_poolable": ws.CachePoolable,
			})
		})

		// Per-workspace opt-in for cross-tenant DISTILL-cache sharing (S0). A
		// separate consent from cache-poolable — distill artifacts are
		// document-derived. Default false (private); cross-tenant serving also
		// requires LENS_DISTILL_POOLABLE_ENABLED + the owner's own opt-in.
		authed.Put("/v1/workspaces/{wsID}/distill-poolable", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				DistillPoolable bool `json:"distill_poolable"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := wsManager.SetDistillPoolable(req.Context(), wsID, in.DistillPoolable); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			ws, _ := wsManager.GetWorkspace(wsID)
			writeJSONOK(w, http.StatusOK, map[string]any{
				"ok":               true,
				"distill_poolable": ws.DistillPoolable,
			})
		})

		authed.Get("/v1/attribution/branch", newAttributionBranchHandler(attrStore))
		authed.Get("/v1/attribution/top", newAttributionTopHandler(attrStore))

		authed.Get("/v1/sessions/{sessionID}", newSessionGetHandler(sessionTracker))

		authed.Get("/v1/sessions/{sessionID}/summary", func(w http.ResponseWriter, req *http.Request) {
			sessionID := chi.URLParam(req, "sessionID")
			// Authz (#146 P3): same IDOR as GET /v1/sessions/{id} — own session only.
			if sess, ok := sessionTracker.GetSession(sessionID); !ok || !callerOwns(req, sess.WorkspaceID) {
				writeJSONErr(w, http.StatusNotFound, "session not found")
				return
			}
			summary := sessionTracker.SummariseSession(req.Context(), sessionID)
			if summary.TurnCount == 0 && summary.StartedAt.IsZero() {
				writeJSONErr(w, http.StatusNotFound, "session not found")
				return
			}
			writeJSONOK(w, http.StatusOK, summary)
		})

		authed.Get("/v1/sessions", newSessionsListHandler(sessionTracker))

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

		authed.Get("/v1/batch/status/{requestID}", newBatchStatusHandler(batchRouter))

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
			wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
			if !ok {
				phase2Forbidden(w)
				return
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
			eff, ok := applyPhase2WSID(req, in.WorkspaceID)
			if !ok {
				phase2Forbidden(w)
				return
			}
			in.WorkspaceID = eff
			summary, err := evalPipeline.RunSuite(req.Context(), in.WorkspaceID, in.Tags)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, summary)
		})

		authed.Get("/v1/eval/runs/{runID}", newEvalRunGetHandler(evalPipeline))

		authed.Get("/v1/eval/runs/{runID}/results", func(w http.ResponseWriter, req *http.Request) {
			runID := chi.URLParam(req, "runID")
			// Authz (#146 P3): same IDOR as GET /v1/eval/runs/{id} — own run only.
			if run, err := evalPipeline.GetRun(req.Context(), runID); err != nil || run == nil || !callerOwns(req, run.WorkspaceID) {
				writeJSONErr(w, http.StatusNotFound, "run not found")
				return
			}
			results, err := evalPipeline.GetResults(req.Context(), runID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, results)
		})

		authed.Get("/v1/eval/runs", newEvalRunsListHandler(evalPipeline))

		// ── Evaluation pipeline: golden datasets, regression runs, A/B
		// significance verdicts, scheduling (Upgrade 17). Workspace-scoped;
		// every path is background/on-demand, off the request hot path.
		authed.Post("/v1/workspaces/{wsID}/eval/datasets", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			ds, err := evalPipeline.CreateDataset(req.Context(), eval.Dataset{
				WorkspaceID: wsID, Name: in.Name, Description: in.Description,
			})
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, ds)
		})

		authed.Get("/v1/workspaces/{wsID}/eval/datasets", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			list, err := evalPipeline.ListDatasets(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		authed.Post("/v1/workspaces/{wsID}/eval/datasets/{dsID}/cases", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			dsID := chi.URLParam(req, "dsID")
			var in eval.TestCase
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			in.WorkspaceID = wsID
			created, err := evalPipeline.AddDatasetCase(req.Context(), dsID, in)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, created)
		})

		authed.Post("/v1/workspaces/{wsID}/eval/run", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				DatasetID string      `json:"dataset_id"`
				Target    eval.Target `json:"target"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			run, err := evalPipeline.RunEval(req.Context(), wsID, in.DatasetID, in.Target)
			if errors.Is(err, eval.ErrCostCapExceeded) {
				// Refuse the run rather than silently burn spend.
				writeJSONErr(w, http.StatusPaymentRequired, err.Error())
				return
			}
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, run)
		})

		authed.Get("/v1/workspaces/{wsID}/eval/runs", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			runs, err := evalPipeline.ListRuns(req.Context(), wsID, 20)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, runs)
		})

		// A/B significance verdict — honest p-value + sample size + plain
		// verdict ("inconclusive" when the data doesn't support a winner).
		authed.Get("/v1/workspaces/{wsID}/eval/ab/{experiment}", func(w http.ResponseWriter, req *http.Request) {
			expID := chi.URLParam(req, "experiment")
			rep, err := abEngine.Significance(req.Context(), expID)
			if err != nil {
				writeJSONErr(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, rep)
		})

		authed.Post("/v1/workspaces/{wsID}/eval/schedules", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in struct {
				DatasetID   string `json:"dataset_id"`
				IntervalSec int    `json:"interval_seconds"`
				Enabled     bool   `json:"enabled"`
				TargetModel string `json:"target_model"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			s, err := evalPipeline.CreateSchedule(req.Context(), eval.Schedule{
				WorkspaceID: wsID, DatasetID: in.DatasetID, IntervalSec: in.IntervalSec,
				Enabled: in.Enabled, TargetModel: in.TargetModel,
			})
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, s)
		})

		authed.Get("/v1/workspaces/{wsID}/eval/schedules", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			list, err := evalPipeline.ListSchedules(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		// ── PoVI receipts (Token Economy Phase 1, Part 1). A node submits its
		// signed receipt; Lens verifies it against the node's registered
		// pubkey, records it for audit, and (only if minting is explicitly
		// enabled) provisionally mints. GET endpoints are audit + status. A
		// receipt is ATTESTATION + TAMPER-EVIDENCE, never proof of honest
		// computation.
		econ.post(authed, "/v1/workspaces/{wsID}/povi/receipts", func(w http.ResponseWriter, req *http.Request) {
			var rec povi.Receipt
			if err := json.NewDecoder(req.Body).Decode(&rec); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			res, err := poviProcessor.Process(req.Context(), rec)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, res)
		})

		econ.get(authed, "/v1/povi/receipts", newPoviReceiptsHandler(poviStore))

		econ.get(authed, "/v1/povi/status", func(w http.ResponseWriter, req *http.Request) {
			total, verified, _ := poviStore.Stats(req.Context())
			writeJSONOK(w, http.StatusOK, map[string]any{
				"minting_enabled":   cfg.POVIMintingEnabled,
				"receipts_total":    total,
				"receipts_verified": verified,
				"attestation_only":  true,
				"warning": "Receipts are ATTESTATION + TAMPER-EVIDENCE, NOT proof of honest computation. " +
					"Provisional receipt-based minting is UNSAFE without stake + challenge (Parts 2/3) and is " +
					"OFF by default; when off, receipts are verified + recorded for audit but NO LENS is minted. " +
					"Separately, the existing trust-based compute mint (RecordServedRequest) remains active and " +
					"unsecured until Part 3 — PoVI is designed to replace it.",
			})
		})

		// ── PoVI staking (Part 2): node collateral that gates minting-
		// eligibility and is slashable (Part 3). Staking does NOT gate serving —
		// an unstaked node still serves, it's just not minting-eligible.
		econ.post(authed, "/v1/povi/nodes/{nodeID}/stake", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if !requireNodeOwnership(w, req, poviStakeManager, nodeID) {
				return
			}
			var in struct {
				Amount float64 `json:"amount"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			st, err := poviStakeManager.Stake(req.Context(), nodeID, in.Amount)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusCreated, st)
		})

		econ.post(authed, "/v1/povi/nodes/{nodeID}/unbond", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if !requireNodeOwnership(w, req, poviStakeManager, nodeID) {
				return
			}
			if err := poviStakeManager.Unbond(req.Context(), nodeID); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			st, _ := poviStakeManager.Get(req.Context(), nodeID)
			writeJSONOK(w, http.StatusOK, st)
		})

		econ.post(authed, "/v1/povi/nodes/{nodeID}/release", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if !requireNodeOwnership(w, req, poviStakeManager, nodeID) {
				return
			}
			if err := poviStakeManager.Release(req.Context(), nodeID); err != nil {
				// Unbonding-not-elapsed is a client error (try again later).
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			st, _ := poviStakeManager.Get(req.Context(), nodeID)
			writeJSONOK(w, http.StatusOK, st)
		})

		econ.get(authed, "/v1/povi/nodes/{nodeID}/stake", func(w http.ResponseWriter, req *http.Request) {
			nodeID := chi.URLParam(req, "nodeID")
			if !requireNodeOwnership(w, req, poviStakeManager, nodeID) {
				return
			}
			st, err := poviStakeManager.Get(req.Context(), nodeID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if st == nil {
				writeJSONOK(w, http.StatusOK, map[string]any{"node_id": nodeID, "staked": false, "eligible": false})
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]any{
				"stake":     st,
				"eligible":  poviStakeManager.IsEligible(req.Context(), nodeID),
				"min_stake": cfg.POVIMinStake,
			})
		})

		econ.get(authed, "/v1/povi/staking/status", func(w http.ResponseWriter, req *http.Request) {
			status, err := poviStakeManager.Status(req.Context())
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, status)
		})

		// ── PoVI challenge-and-slash (Part 3). Lens publishes its challenge
		// pubkey for nodes to pin; audits challenges; allows a manual challenge
		// (admin/testing); and reports the honest economic-deterrent summary.
		econ.get(authed, "/v1/povi/pubkey", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, map[string]string{
				"ed25519_pubkey": povi.EncodePublicKey(lensChallengePub),
				"purpose":        "PoVI Part-3 challenge signing — pin this to verify challenges are from Lens",
			})
		})

		econ.get(authed, "/v1/povi/challenges", func(w http.ResponseWriter, req *http.Request) {
			// Authz (#146 P3): node-keyed read. A specific node must be owned by
			// the caller; the unfiltered list (no node) is admin-only — a tenant
			// can't enumerate every node's challenges.
			node := req.URL.Query().Get("node")
			if node == "" {
				if _, isAdmin := auth.WorkspaceIdentity(req.Context()); !isAdmin {
					writeJSONErr(w, http.StatusForbidden, "forbidden: specify a node you own, or use an admin credential")
					return
				}
			} else if !requireNodeOwnership(w, req, poviStakeManager, node) {
				return
			}
			list, err := challengeStore.List(req.Context(), node)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		econ.get(authed, "/v1/povi/challenges/{id}", newChallengeGetHandler(challengeStore))

		econ.post(authed, "/v1/povi/challenges/issue", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				RequestID string `json:"request_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			rec, err := poviStore.GetReceipt(req.Context(), in.RequestID)
			if err != nil || rec == nil {
				writeJSONErr(w, http.StatusNotFound, "receipt not found")
				return
			}
			ch, err := poviChallenger.Challenge(req.Context(), *rec)
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, ch)
		})

		econ.get(authed, "/v1/povi/security/status", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, map[string]any{
				"minting_enabled":         cfg.POVIMintingEnabled,
				"trustful_compute_mint":   cfg.TrustfulComputeMintEnabled,
				"challenge_rate":          cfg.POVIChallengeRate,
				"slash_fraction":          cfg.POVISlashFraction,
				"positions_per_challenge": poviChallenger.PositionsPerChallenge(),
				"min_stake":               cfg.POVIMinStake,
				"model": "PROBABILISTIC, NOT ABSOLUTE. Security is economic: a rational node with stake at risk " +
					"finds cheating unprofitable when expected_cost = P(challenge) × slash_amount > gain_from_cheating. " +
					"A low challenge rate leaves some bad receipts unchallenged — this is NOT a cryptographic guarantee " +
					"of honest computation; receipts are attestation + tamper-evidence, and challenge-and-slash makes " +
					"cheating economically irrational, not impossible.",
				"retirement_note": "With Parts 1+2+3 in place, receipt minting is now SAFE to enable " +
					"(LENS_POVI_MINTING_ENABLED=true) — operator decision, default OFF. Once enabled, retire the legacy " +
					"trust-mint with LENS_TRUSTFUL_COMPUTE_MINT_ENABLED=false.",
			})
		})

		// Guardrails: per-workspace safety policy + pre-flight check.
		// Policy changes apply immediately; the engine reads on every
		// proxy request and there is no per-request policy cache.
		authed.Get("/v1/guardrails/policy", newGuardrailsPolicyGetHandler(guardrailsEngine))

		authed.Put("/v1/guardrails/policy", newGuardrailsPolicyPutHandler(guardrailsEngine))

		authed.Post("/v1/guardrails/check", func(w http.ResponseWriter, req *http.Request) {
			var in struct {
				Prompt      string `json:"prompt"`
				WorkspaceID string `json:"workspace_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			eff, ok := applyPhase2WSID(req, in.WorkspaceID)
			if !ok {
				phase2Forbidden(w)
				return
			}
			in.WorkspaceID = eff
			result := guardrailsEngine.Check(req.Context(), in.WorkspaceID, in.Prompt, nil)
			writeJSONOK(w, http.StatusOK, result)
		})

		// ─── workspace-scoped guardrails config (Upgrade 13) ───
		// REST surface per workspace. POST/PATCH replace the policy; DELETE
		// reverts to the default. The /test dry-run previews input + output
		// triggers without affecting traffic (output preview runs even when
		// the output stage is off).
		authed.Get("/v1/workspaces/{wsID}/guardrails", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, guardrailsEngine.GetPolicy(chi.URLParam(req, "wsID")))
		})
		setGuardrails := func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			var in guardrails.GuardrailPolicy
			if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			if err := guardrailsEngine.SetPolicy(req.Context(), wsID, in); err != nil {
				// Policy is live on this node (map-first) but did not persist — tell
				// the admin so they retry (it won't survive a restart otherwise).
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, guardrailsEngine.GetPolicy(wsID))
		}
		authed.Post("/v1/workspaces/{wsID}/guardrails", setGuardrails)
		authed.Patch("/v1/workspaces/{wsID}/guardrails", setGuardrails)
		authed.Delete("/v1/workspaces/{wsID}/guardrails", func(w http.ResponseWriter, req *http.Request) {
			if err := guardrailsEngine.DeletePolicy(req.Context(), chi.URLParam(req, "wsID")); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})
		authed.Get("/v1/workspaces/{wsID}/guardrails/test", func(w http.ResponseWriter, req *http.Request) {
			wsID := chi.URLParam(req, "wsID")
			q := req.URL.Query()
			out := map[string]any{}
			if p := q.Get("prompt"); p != "" {
				out["input"] = guardrailsEngine.Check(req.Context(), wsID, p, nil)
			}
			if c := q.Get("content"); c != "" {
				out["output"] = guardrailsEngine.CheckOutputPreview(wsID, c)
			}
			writeJSONOK(w, http.StatusOK, out)
		})

		// Audit export — synchronous download. The exporter streams rows
		// straight to the http.ResponseWriter so a 100k-record CSV never
		// materialises in memory. Format defaults to JSON; query params
		// drive the WHERE filters and the LIMIT cap.
		authed.Get("/v1/audit/export", newAuditExportHandler(auditExporter))

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
			// Authz (#146): non-admin scoped to own workspace; admin honors the
			// filter (empty workspace = all tenants, admin-only).
			effWS, _, ok := effectiveWorkspaceID(req, in.Filter.WorkspaceID)
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
			in.Filter.WorkspaceID = effWS
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
		authed.Post("/v1/api/keys/pool", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
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
		})))

		authed.Get("/v1/api/keys/pool", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, keyPool.Stats())
		})

		authed.Delete("/v1/api/keys/pool/{keyID}", requireAdmin(authManager, http.HandlerFunc(newPoolKeyDeleteHandler(keyPool))))

		// Fallback chain inspection and override. The router is in-memory;
		// updates here are not persisted — restarting the binary resets
		// chains to the defaults.
		authed.Get("/v1/api/fallback/chains", func(w http.ResponseWriter, req *http.Request) {
			writeJSONOK(w, http.StatusOK, fallbackRouter.AllChains())
		})

		authed.Put("/v1/api/fallback/chains/{provider}", requireAdmin(authManager, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			provider := chi.URLParam(req, "provider")
			var targets []fallback.FallbackTarget
			if err := json.NewDecoder(req.Body).Decode(&targets); err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
				return
			}
			fallbackRouter.SetChain(provider, targets)
			writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
		})))

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
			wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
			if !ok {
				phase2Forbidden(w)
				return
			}
			list, err := promptManager.List(req.Context(), wsID)
			if err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSONOK(w, http.StatusOK, list)
		})

		authed.Get("/v1/prompts/{name}", newPromptGetHandler(promptManager))

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
			// Authz (#146): a non-admin may mutate only its OWN prompt; admin
			// honors the body (empty → "default").
			eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
			in.WorkspaceID = eff
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
			wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
			if !ok {
				phase2Forbidden(w)
				return
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
			// Authz (#146): a non-admin may roll back only its OWN prompt; admin
			// honors the body (empty → "default").
			eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)
			if !ok {
				writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
				return
			}
			in.WorkspaceID = eff
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
			wsID, ok := applyPhase2WSID(req, req.URL.Query().Get("workspace_id"))
			if !ok {
				phase2Forbidden(w)
				return
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

	// Verify the TLS cert cache directory exists and is writable before
	// handing control to the autocert manager.  Failing here is cheap;
	// failing at the first TLS handshake burns Let's Encrypt rate-limit quota.
	if cfg.TLSDomain != "" {
		if err := checkTLSCacheDir(cfg.TLSCacheDir); err != nil {
			return fmt.Errorf("TLS setup: %w", err)
		}
	}

	servers, serverErr := startServers(cfg, r, logger)

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

	if err := servers.shutdown(drainCtx); err != nil {
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

// loadOrGenChallengeKey returns Lens's ed25519 challenge-signing keypair. A
// base64 seed/private key (LENS_POVI_CHALLENGE_KEY) gives a STABLE key so a Lens
// restart doesn't invalidate the pubkey nodes have pinned (which would make
// honest nodes fail challenges). Empty → ephemeral key + a loud warning.
func loadOrGenChallengeKey(b64 string, logger *slog.Logger) (ed25519.PublicKey, ed25519.PrivateKey) {
	if b64 != "" {
		if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
			switch len(raw) {
			case ed25519.SeedSize:
				priv := ed25519.NewKeyFromSeed(raw)
				return priv.Public().(ed25519.PublicKey), priv
			case ed25519.PrivateKeySize:
				priv := ed25519.PrivateKey(raw)
				return priv.Public().(ed25519.PublicKey), priv
			}
		}
		logger.Warn("LENS_POVI_CHALLENGE_KEY is set but not a valid base64 ed25519 seed/key — generating an ephemeral key instead")
	} else {
		logger.Warn("PoVI challenge key generated EPHEMERALLY — set LENS_POVI_CHALLENGE_KEY (base64 ed25519 seed) in production; a restart otherwise invalidates pinned node pubkeys and would fail honest challenges")
	}
	pub, priv, _ := povi.GenerateNodeKey()
	return pub, priv
}

func writeJSONOK(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	writeJSONOK(w, status, map[string]string{"error": msg})
}

// ─── tenant isolation (cross-tenant BOLA guard) ──────────────────
//
// The authed group authenticates any valid credential, but the
// /v1/workspaces/{wsID}/... handlers used to trust the {wsID} path segment
// outright. A holder of workspace A's key could therefore read/modify
// workspace B's config and budgets and — worst of all — mint API keys for
// workspace B (full tenant takeover). These helpers enforce that a caller may
// only act on its own workspace, with the global admin key exempted.

// workspaceAuthorized is the pure tenant-isolation decision: the global admin
// (isAdmin) may act on any workspace; every other caller may act only on the
// workspace its credential belongs to. Deny-by-default — an empty caller
// workspace never matches a non-empty target.
func workspaceAuthorized(callerWorkspaceID string, isAdmin bool, targetWorkspaceID string) bool {
	if isAdmin {
		return true
	}
	return callerWorkspaceID != "" && callerWorkspaceID == targetWorkspaceID
}

// callerWorkspaceID resolves the authenticated caller's workspace and whether
// it is the global admin, handling both AuthMiddleware credential paths:
//   - DB workspace/team keys take the fast path, which populates only the
//     APIKey context slot (auth.GetAPIKey). These are never admin.
//   - The global admin key and JWT bearer tokens take the Manager fallback,
//     which populates the AuthContext slot (auth.GetAuthContext). Only the
//     global key carries IsAdmin. NOTE: both the global key and JWTs synthesize
//     an APIKey with ID=="global", so APIKey.ID is NOT a safe admin signal —
//     AuthContext.IsAdmin is the authoritative one, which is why it is checked
//     first.
func callerWorkspaceID(ctx context.Context) (workspaceID string, isAdmin bool) {
	if actx := auth.GetAuthContext(ctx); actx != nil {
		return actx.WorkspaceID, actx.IsAdmin
	}
	if k := auth.GetAPIKey(ctx); k != nil {
		return k.WorkspaceID, false
	}
	return "", false
}

// effectiveWorkspaceID resolves which workspace a NON-{wsID}-path route may act
// on, deriving identity from the authenticated credential rather than caller
// input. It closes the cross-tenant authorization cluster (#146): routes that
// took workspace_id from a query/body param trusted attacker-chosen input,
// because workspaceIsolationMiddleware only guards the {wsID} PATH segment.
//
//   - A NON-ADMIN caller is ALWAYS forced to its own workspace; `requested`
//     (whatever the query/body said) is ignored — it can never name another
//     tenant. An honest caller naming its own workspace is unaffected.
//   - The global ADMIN honors `requested`; an empty value preserves each
//     handler's admin-wide semantics (e.g. audit export's all-tenant dump).
//
// ok is false ONLY when a non-admin has no resolvable workspace — the caller
// must 403 rather than fall through to a shared/"default"/all-tenant path.
func effectiveWorkspaceID(r *http.Request, requested string) (wsID string, isAdmin bool, ok bool) {
	caller, admin := callerWorkspaceID(r.Context())
	if admin {
		return requested, true, true
	}
	if caller == "" {
		return "", false, false
	}
	return caller, false, true
}

// workspaceIsolationMiddleware enforces tenant isolation on every routed
// request that carries a {wsID} path param. Routes without a {wsID} param
// (auth, catalog, proxy, global status) pass straight through. chi resolves
// URL params before a group's Use chain executes, so chi.URLParam(r,"wsID") is
// populated here.
func workspaceIsolationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wsID := chi.URLParam(r, "wsID"); wsID != "" {
			callerWS, isAdmin := callerWorkspaceID(r.Context())
			if !workspaceAuthorized(callerWS, isAdmin, wsID) {
				writeJSONErr(w, http.StatusForbidden, "forbidden: credential not authorized for this workspace")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
