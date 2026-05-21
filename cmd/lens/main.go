package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/talyvor/lens/internal/ab"
	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/api"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/proxy"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/templates"
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

	l := learner.New(nc, pool)
	go l.StartBackground(ctx)

	p := proxy.New(exactCache, semanticCache, openAIEmbedder, promptCompressor, modelRouter, piiDetector, alertManager, templateDetector, qualityScorer, abTester, branchTracker, wsManager, lr, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey, l)

	keyStore := auth.New(pool)
	if err := keyStore.LoadAll(ctx); err != nil {
		logger.Warn("auth: LoadAll failed", slog.String("err", err.Error()))
	}

	r := chi.NewRouter()
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

	// Everything else sits behind the API-key middleware. chi.Group inherits
	// middleware only for routes registered inside its closure.
	r.Group(func(authed chi.Router) {
		authed.Use(auth.AuthMiddleware(keyStore))

		apiServer.MountAuthenticated(authed)

		authed.Post("/v1/proxy/openai/*", p.HandleOpenAI)
		authed.Post("/v1/proxy/anthropic/*", p.HandleAnthropic)

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
	return tp, nil
}
