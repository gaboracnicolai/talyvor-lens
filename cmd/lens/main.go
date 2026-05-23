package main

import (
	"context"
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
	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/batch"
	"github.com/talyvor/lens/internal/budget"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/dashboard"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/mcp"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/prompts"
	"github.com/talyvor/lens/internal/proxy"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/internal/ratelimit"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/templates"
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
	branchTracker := attribution.New(pool)
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
	injectionDetector := injection.New(injection.DefaultPolicy())
	budgetEnforcer := budget.New(pool, budget.BudgetPolicy{MaxOutputTokens: 4096})
	batchRouter := batch.New(pool, cfg.AnthropicAPIKey)
	go batchRouter.StartPoller(ctx)
	sessionTracker := session.New(pool)
	go sessionTracker.StartCleanup(ctx)

	promptManager := prompts.New(pool)

	l := learner.New(nc, pool)
	go l.StartBackground(ctx)

	cacheWarmer := warmer.New(pool, l, exactCache, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey)
	go cacheWarmer.Start(ctx, 1*time.Hour)

	p := proxy.New(exactCache, semanticCache, openAIEmbedder, promptCompressor, modelRouter, piiDetector, alertManager, templateDetector, qualityScorer, abTester, branchTracker, wsManager, lr, injectionDetector, budgetEnforcer, batchRouter, sessionTracker, promptManager, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey, cfg.GoogleAPIKey, l)

	keyStore := auth.New(pool)
	if err := keyStore.LoadAll(ctx); err != nil {
		logger.Warn("auth: LoadAll failed", slog.String("err", err.Error()))
	}
	rateLimiter := ratelimit.New(redisClient, ratelimit.DefaultRules())

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

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	r.Handle("/metrics", metrics.Handler())

	apiServer := api.NewServer(
		pool, redisClient, nc, exactCache, l,
		alertManager, abTester, branchTracker, wsManager, lr,
		"0.1.0",
	)
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
	r.Get("/", dashHandler.RedirectRoot)

	// Everything else sits behind the API-key middleware. chi.Group inherits
	// middleware only for routes registered inside its closure.
	r.Group(func(authed chi.Router) {
		// Auth must run first so the rate-limiter sees the key/workspace
		// that AuthMiddleware just stamped onto the request.
		authed.Use(auth.AuthMiddleware(keyStore))
		authed.Use(ratelimit.RateLimitMiddleware(rateLimiter))

		apiServer.MountAuthenticated(authed)

		authed.Post("/v1/proxy/openai/*", p.HandleOpenAI)
		authed.Post("/v1/proxy/anthropic/*", p.HandleAnthropic)
		authed.Post("/v1/proxy/google/*", p.HandleGoogle)

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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
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
