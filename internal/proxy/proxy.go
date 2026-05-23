package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/talyvor/lens/internal/ab"
	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/batch"
	"github.com/talyvor/lens/internal/budget"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/keypool"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/prompts"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/internal/retry"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/templates"
	"github.com/talyvor/lens/internal/workspace"
)

const (
	maxBodyBytes                  = 4 << 20 // 4 MiB
	openAIChatURL                 = "https://api.openai.com/v1/chat/completions"
	anthropicMessageURL           = "https://api.anthropic.com/v1/messages"
	googleGenerativeLanguageURL   = "https://generativelanguage.googleapis.com"
	upstreamTimeout               = 120 * time.Second
	defaultWorkspaceID            = "default"
)

// alertSink is the subset of *alerts.AlertManager that proxy.serve()
// touches. Defined locally so tests can drop in a counter mock without
// pulling in the full pgxpool / NATS stack the real manager needs.
type alertSink interface {
	IsCircuitOpen(team, feature string) bool
	GetDowngradeModel(provider, model string) string
	RecordSpend(ctx context.Context, team, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID string) error
}

type Proxy struct {
	exact            *cache.ExactCache
	semantic         *cache.SemanticCache
	embedder         cache.Embedder
	compressor       *compressor.Compressor
	router           *router.Router
	piiDetector      *pii.Detector
	alertManager     alertSink
	templateDetector *templates.TemplateDetector
	scorer           *quality.Scorer
	abTester         *ab.Tester
	tracker           *attribution.Tracker
	workspaceManager  *workspace.Manager
	localRouter       *localrouter.LocalRouter
	injectionDetector *injection.Detector
	budgetEnforcer    *budget.Enforcer
	batchRouter       *batch.BatchRouter
	sessionTracker    *session.SessionTracker
	promptManager     *prompts.Manager
	fallbackRouter    *fallback.FallbackRouter
	keyPool           *keypool.Pool
	auditExporter     *audit.Exporter
	retryConfig       retry.Config
	httpClient        *http.Client
	openAIKey         string
	anthropicKey      string
	googleKey         string
	learner           *learner.Learner

	// Upstream URLs are unexported and defaulted so tests can swap them
	// for an httptest server without leaking config to callers.
	openAIURL    string
	anthropicURL string
	googleURL    string

	// Bedrock-specific state. bedrockURL is empty in production (URL is
	// computed from region); tests set it to an httptest base URL. The
	// config is set via SetBedrockConfig after construction so New's
	// already-long parameter list doesn't grow further.
	bedrockConfig BedrockConfig
	bedrockURL    string
}

// New constructs a Proxy. The learner is variadic so callers that don't
// need usage analytics still compile; production wires a *learner.Learner
// as the last argument.
func New(
	exactCache *cache.ExactCache,
	semanticCache *cache.SemanticCache,
	embedder cache.Embedder,
	compressorImpl *compressor.Compressor,
	routerImpl *router.Router,
	piiDetector *pii.Detector,
	alertManager *alerts.AlertManager,
	templateDetector *templates.TemplateDetector,
	scorer *quality.Scorer,
	abTester *ab.Tester,
	tracker *attribution.Tracker,
	workspaceManager *workspace.Manager,
	localRouter *localrouter.LocalRouter,
	injectionDetector *injection.Detector,
	budgetEnforcer *budget.Enforcer,
	batchRouter *batch.BatchRouter,
	sessionTracker *session.SessionTracker,
	promptManager *prompts.Manager,
	fallbackRouter *fallback.FallbackRouter,
	keyPool *keypool.Pool,
	auditExporter *audit.Exporter,
	openAIKey string,
	anthropicKey string,
	googleKey string,
	learners ...*learner.Learner,
) *Proxy {
	p := &Proxy{
		exact:            exactCache,
		semantic:         semanticCache,
		embedder:         embedder,
		compressor:       compressorImpl,
		router:           routerImpl,
		piiDetector:      piiDetector,
		templateDetector: templateDetector,
		scorer:           scorer,
		abTester:         abTester,
		tracker:          tracker,
		workspaceManager:  workspaceManager,
		localRouter:       localRouter,
		injectionDetector: injectionDetector,
		budgetEnforcer:    budgetEnforcer,
		batchRouter:       batchRouter,
		sessionTracker:    sessionTracker,
		promptManager:     promptManager,
		fallbackRouter:    fallbackRouter,
		keyPool:           keyPool,
		auditExporter:     auditExporter,
		retryConfig:       retry.DefaultConfig(),
		httpClient:        &http.Client{Timeout: upstreamTimeout},
		openAIKey:         openAIKey,
		anthropicKey:      anthropicKey,
		googleKey:         googleKey,
		openAIURL:         openAIChatURL,
		anthropicURL:      anthropicMessageURL,
		googleURL:         googleGenerativeLanguageURL,
	}
	if len(learners) > 0 {
		p.learner = learners[0]
	}
	// Guard the typed-nil interface trap: assign the concrete pointer
	// only when it isn't nil so `p.alertManager != nil` keeps working.
	if alertManager != nil {
		p.alertManager = alertManager
	}
	return p
}

// SetAlertSink lets tests inject a counter mock implementing alertSink.
// Production never calls this — main.go wires a real *alerts.AlertManager
// through New(). Kept unexported in spirit (tests are in-package).
func (p *Proxy) setAlertSink(sink alertSink) {
	p.alertManager = sink
}

// providerConfig holds the per-provider knobs HandleOpenAI/HandleAnthropic/
// HandleGoogle differ on. Everything else is shared in serve(). The URL
// is a function of the model so Gemini's path-style routing fits cleanly.
type providerConfig struct {
	name              string
	upstreamURLFn     func(model string) string
	setAuth           func(*http.Request)
	translateRequest  func(body []byte) ([]byte, error)
	translateResponse func(body []byte, model string) ([]byte, error)
}

func (p *Proxy) HandleOpenAI(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, providerConfig{
		name:          "openai",
		upstreamURLFn: func(string) string { return p.openAIURL },
		setAuth: func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+p.openAIKey)
		},
	})
}

func (p *Proxy) HandleAnthropic(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, providerConfig{
		name:          "anthropic",
		upstreamURLFn: func(string) string { return p.anthropicURL },
		setAuth: func(req *http.Request) {
			req.Header.Set("x-api-key", p.anthropicKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		},
	})
}

// HandleGoogle proxies an OpenAI-shaped request through to Gemini's
// generateContent endpoint, translating the body in and the response
// back out so downstream caching / scoring / cost-attribution code
// treats the request indistinguishably from OpenAI or Anthropic.
func (p *Proxy) HandleGoogle(w http.ResponseWriter, r *http.Request) {
	if p.googleKey == "" {
		writeError(w, http.StatusServiceUnavailable, "Google API key not configured")
		return
	}
	p.serve(w, r, providerConfig{
		name: "google",
		upstreamURLFn: func(model string) string {
			return p.googleURL + "/v1beta/models/" + model + ":generateContent?key=" + url.QueryEscape(p.googleKey)
		},
		// Gemini uses ?key=<value>; nothing to set on the request headers.
		setAuth: func(*http.Request) {},
		// Adapter drops the model return value — serve() already has the
		// upstream model in scope; we only need the translated body here.
		translateRequest: func(body []byte) ([]byte, error) {
			out, _, err := translateToGemini(body)
			return out, err
		},
		translateResponse: translateFromGemini,
	})
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, cfg providerConfig) {
	ctx := r.Context()

	body, err := readLimitedBody(r, maxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 4MB limit")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}

	model, prompt, err := extractPrompt(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	// Batch dispatch: when the caller flips X-Talyvor-Batch we route the
	// whole request through Anthropic's async batches endpoint instead of
	// the normal proxy flow. We deliberately run this BEFORE any workspace
	// or cache work — the batch endpoint replies 202 immediately and the
	// background poller picks up the response hours later.
	if p.batchRouter != nil && r.Header.Get("X-Talyvor-Batch") == "true" {
		batchBody := withBatchEligibleFlag(body)
		preWsID := r.Header.Get("X-Talyvor-Workspace")
		if preWsID == "" {
			preWsID = defaultWorkspaceID
		}
		if elig := p.batchRouter.IsEligible(batchBody, preWsID); elig.Eligible {
			job, err := p.batchRouter.Submit(ctx, preWsID, model, prompt, batchBody)
			if err == nil {
				writeJSON(w, http.StatusAccepted, map[string]any{
					"request_id":           job.RequestID,
					"batch_id":             job.ID,
					"status":               string(job.Status),
					"estimated_completion": "within 24 hours",
					"cost_reduction":       "50%",
				})
				metrics.RequestsTotal.WithLabelValues(cfg.name, "batched").Inc()
				return
			}
			slog.Warn("batch: Submit failed; falling through to live request",
				slog.String("err", err.Error()),
			)
		} else {
			slog.Info("batch: not eligible; falling through",
				slog.String("reason", elig.Reason),
			)
		}
	}

	// Workspace identification + logging policy come BEFORE the workspace
	// policy gate so even a 403 response carries the X-Talyvor-Logging
	// header. wsID resolution (ExtractWorkspaceID) is cheap and pure —
	// header read with a "default" fallback — so doing it ahead of the
	// CheckPolicy call has no downside.
	wsID := defaultWorkspaceID
	if p.workspaceManager != nil {
		wsID = p.workspaceManager.ExtractWorkspaceID(r)
	}

	// Request ID stamped here so every downstream artefact — token_events
	// row, structured log, span attribute — can be correlated back to
	// the same HTTP call. Generated fresh per request; clients can also
	// pass an X-Talyvor-Request-ID header to retain their own.
	requestID := r.Header.Get("X-Talyvor-Request-ID")
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Talyvor-Request-ID", requestID)

	// Per-workspace logging policy. Decided once per request and applied
	// at each observability write below. Default (metadata) preserves
	// costs/tokens but drops prompt text; `none` skips every DB write;
	// `full` keeps the historic behaviour. Security checks (PII,
	// injection) run regardless of policy. Header is set BEFORE every
	// early-return path (workspace 403, injection 400, budget 400, cache
	// hit, SSE replay) so downstream consumers always see the policy
	// that governed the response.
	loggingPolicy := workspace.LoggingMetadata
	if p.workspaceManager != nil {
		loggingPolicy = p.workspaceManager.GetLoggingPolicy(wsID)
	}
	w.Header().Set("X-Talyvor-Logging", string(loggingPolicy))

	// Workspace policy gate. The workspace decision happens before any
	// cache lookup so a blocked workspace can't even read someone else's
	// cached response — the request returns 403 immediately. Cache
	// isolation downstream is then achieved by prefixing the prompt with
	// the workspace ID before it reaches the cache layer.
	cachePrompt := prompt
	if p.workspaceManager != nil {
		policy := p.workspaceManager.CheckPolicy(ctx, wsID, cfg.name, model, len(prompt)/4)
		if !policy.Allowed {
			writeError(w, http.StatusForbidden, policy.Violation)
			metrics.RequestsTotal.WithLabelValues(cfg.name, "workspace_blocked").Inc()
			return
		}
		cachePrompt = wsID + ":" + prompt
	}

	// Session pickup — header-driven, optional. Empty sessionID means
	// the caller isn't tracking sessions; the entire feature is skipped.
	sessionID := r.Header.Get("X-Talyvor-Session")
	agentName := r.Header.Get("X-Talyvor-Agent")
	if agentName == "" {
		agentName = "default"
	}
	var sess *session.Session
	if sessionID != "" && p.sessionTracker != nil {
		sess = p.sessionTracker.GetOrCreate(ctx, sessionID, wsID, agentName)
	}

	// Extract any incoming W3C trace context BEFORE we start our own
	// span. otelhttp middleware already does this in production, but
	// extracting again here is idempotent and keeps tests + direct
	// handler invocations correct.
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))

	// streaming flag is needed both for the cache replay branch below
	// and for the OTel span attributes — compute it once here.
	streaming := streamRequested(body)

	// Start the proxy span. Attributes carry the safe metadata —
	// provider/model/workspace/stream — and never the prompt content.
	ctx, span := otel.Tracer("lens/proxy").Start(ctx, "proxy.serve",
		trace.WithAttributes(
			attribute.String("lens.provider", cfg.name),
			attribute.String("lens.model", model),
			attribute.String("lens.workspace", wsID),
			attribute.Bool("lens.stream", streaming),
		),
	)
	defer span.End()

	// Set the response header right after the span is live so it persists
	// even if a cache hit short-circuits the request below.
	if sc := trace.SpanFromContext(ctx).SpanContext(); sc.IsValid() {
		w.Header().Set("X-Talyvor-Trace-ID", sc.TraceID().String())
	}

	// Team / feature attribution for cost accounting and circuit breakers.
	// Both default to "" when callers don't supply the headers; the alert
	// manager treats empty values as a distinct attribution bucket.
	team := r.Header.Get("X-Talyvor-Team")
	feature := r.Header.Get("X-Talyvor-Feature")

	// Branch / PR / commit attribution. Extracted up front so we can set
	// response headers before WriteHeader; the actual DB write happens
	// after the response body is sent so a slow INSERT can't hold up the
	// client.
	var attr attribution.Attribution
	if p.tracker != nil {
		attr = p.tracker.ExtractAttribution(r)
	}
	willAttribute := p.tracker != nil && (attr.Branch != "" || attr.PRNumber != "")

	// Template detection runs BEFORE PII detection so we count hits against
	// the actual system prompt the caller sent. When the system prompt
	// contains PII we record only the hash + a redacted placeholder so we
	// never persist the raw value to prompt_templates. Once a template
	// crosses the pin threshold we rewrite the body to opt the upstream
	// call into Anthropic's prompt-caching feature; OpenAI caches long
	// system prompts automatically, so its hook is a no-op.
	if p.templateDetector != nil {
		if sysPrompt, found := p.templateDetector.ExtractSystemPrompt(body); found {
			contentForRecord := sysPrompt
			if p.piiDetector != nil {
				if rr := p.piiDetector.Detect(sysPrompt); rr.WasRedacted {
					contentForRecord = "[REDACTED-PII]"
				}
			}
			tmpl, pinned := p.templateDetector.RecordAndPin(ctx, contentForRecord, cfg.name)
			if pinned && cfg.name == "anthropic" {
				if rewritten, err := p.templateDetector.ApplyAnthropicCaching(body, tmpl); err == nil {
					body = rewritten
				}
			}
		}
	}

	// Named-prompt resolution. Swap any "lens:prompt:<name>" system
	// message for the active prompt body stored under that name in this
	// workspace. Runs after template detection (which already records a
	// hash of the placeholder system prompt) and before PII detection so
	// the PII gate scans the resolved content. Body is only mutated when
	// Resolve actually found a match.
	if p.promptManager != nil {
		if resolved, err := p.promptManager.Resolve(ctx, body, wsID); err == nil && !bytes.Equal(resolved, body) {
			body = resolved
			w.Header().Set("X-Talyvor-Prompt-Resolved", "true")
		}
	}

	// PII gate. When the prompt contains PII we never read from or write
	// to the cache — one user's PII must never be served to another user.
	// The original (unredacted) prompt is still forwarded to the LLM; only
	// caching is skipped. The redacted form is what we expose in logs and
	// in the token event.
	piiDetected := false
	var piiTypes []string
	var redactedPrompt string
	if p.piiDetector != nil {
		piiResult := p.piiDetector.Detect(prompt)
		if !p.piiDetector.IsSafeToCache(piiResult) {
			piiDetected = true
			piiTypes = piiResult.Types
			redactedPrompt = piiResult.Redacted
			w.Header().Set("X-Talyvor-PII-Detected", "true")
			slog.Info("PII detected, skipping cache",
				slog.String("provider", cfg.name),
				slog.Any("types", piiTypes),
			)
			metrics.RequestsTotal.WithLabelValues(cfg.name, "pii_skip_cache").Inc()
		}
	}

	// Prompt-injection check. Runs after PII (which may have edited the
	// prompt for caching purposes) but on the ORIGINAL prompt the user
	// sent — we want to detect attempted injections regardless of what
	// the cache key looks like. Block stops the request before anything
	// reaches the LLM; Warn just stamps a header and logs the patterns.
	if p.injectionDetector != nil {
		ir := p.injectionDetector.Detect(prompt)
		switch ir.Action {
		case injection.ActionBlock:
			slog.Warn("proxy: injection blocked",
				slog.String("provider", cfg.name),
				slog.Any("patterns", ir.Patterns),
				slog.Float64("risk_score", ir.RiskScore),
			)
			metrics.RequestsTotal.WithLabelValues(cfg.name, "injection_blocked").Inc()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":      "prompt injection detected",
				"risk_score": ir.RiskScore,
			})
			return
		case injection.ActionWarn:
			slog.Warn("proxy: injection warning",
				slog.String("provider", cfg.name),
				slog.Any("patterns", ir.Patterns),
				slog.Float64("risk_score", ir.RiskScore),
			)
			w.Header().Set("X-Talyvor-Injection-Warning", "true")
		}
	}

	// Token-budget enforcement: rewrite the body in place so max_tokens
	// honours the workspace's policy before anything reaches the LLM.
	// Parse errors leave the body untouched — extractPrompt below would
	// have surfaced the same problem anyway.
	if p.budgetEnforcer != nil {
		if newBody, br, err := p.budgetEnforcer.EnforceOnBody(ctx, wsID, body); err == nil && br.Rewritten {
			body = newBody
			w.Header().Set("X-Talyvor-Budget-Applied", "true")
			slog.Info("proxy: budget enforced",
				slog.String("workspace_id", wsID),
				slog.String("reason", br.Reason),
			)
		}
	}

	if !piiDetected {
		var cached []byte
		var layer string
		span.AddEvent("cache.check.exact")
		if c := p.tryExact(ctx, cfg.name, model, cachePrompt); c != nil {
			cached, layer = c, "cache_hit_exact"
			span.AddEvent("cache.hit.exact")
		} else {
			span.AddEvent("cache.check.semantic")
			if c := p.trySemantic(ctx, cfg.name, model, cachePrompt); c != nil {
				cached, layer = c, "cache_hit_semantic"
				span.AddEvent("cache.hit.semantic")
			}
		}
		if cached != nil {
			// Record the session turn first so the headers below reflect
			// the count + cost AFTER this turn lands. Cache hits have
			// zero cost.
			if sess != nil {
				p.recordSessionTurn(ctx, sessionID, prompt, string(cached), model, 0, true)
				setSessionHeaders(w, p, sessionID)
			}
			if streaming {
				// SSE replay: synthesises the provider's streaming wire
				// format from the cached JSON so strict SSE clients can
				// consume cache hits the same way they consume live
				// responses. On parse failure we fall through and let
				// the regular streaming path call the LLM.
				if err := replayAsSSE(w, cfg.name, cached); err == nil {
					metrics.RequestsTotal.WithLabelValues(cfg.name, layer).Inc()
					span.SetAttributes(
						attribute.Bool("lens.cached", true),
						attribute.Float64("lens.cost_usd", 0),
					)
					span.SetStatus(codes.Ok, "")
					return
				}
				slog.Warn("proxy: cached payload not replayable as SSE; falling through to LLM",
					slog.String("provider", cfg.name),
				)
			} else {
				writeBytes(w, http.StatusOK, cached)
				metrics.RequestsTotal.WithLabelValues(cfg.name, layer).Inc()
				span.SetAttributes(
					attribute.Bool("lens.cached", true),
					attribute.Float64("lens.cost_usd", 0),
				)
				span.SetStatus(codes.Ok, "")
				return
			}
		}
	}

	// Local-model short-circuit: simple queries from the default
	// workspace can be served by a local Ollama instance for free. On
	// any failure we fall through to the regular cloud path — local
	// routing must never break the main request.
	if p.tryLocalRouting(w, ctx, cfg.name, model, prompt, cachePrompt, wsID, team, feature, sessionID, requestID, piiDetected, redactedPrompt) {
		return
	}

	// Streaming path: detected by "stream": true in the request JSON. The
	// stream handler forwards SSE chunks unbuffered, then caches the
	// assembled response after the upstream stream completes. We skip the
	// compression + routing path for streams since that would rewrite the
	// body and break wire-compatibility with the live SSE.
	if streaming {
		sh := &StreamHandler{proxy: p}
		var serr error
		if cfg.name == "openai" {
			serr = sh.ServeOpenAI(w, r, cfg.name, model, prompt, cachePrompt, body, piiDetected)
		} else {
			serr = sh.ServeAnthropic(w, r, cfg.name, model, prompt, cachePrompt, body, piiDetected)
		}
		if serr != nil {
			metrics.RequestsTotal.WithLabelValues(cfg.name, "stream_error").Inc()
			return
		}
		metrics.RequestsTotal.WithLabelValues(cfg.name, "streamed").Inc()
		return
	}

	// Compress the prompt before forwarding upstream. Cache lookups above
	// still key on the uncompressed prompt so repeat callers hit cache.
	compressedPrompt := prompt
	var savingsPct float64
	if p.compressor != nil {
		result := p.compressor.Compress(ctx, prompt)
		compressedPrompt = result.CompressedPrompt
		savingsPct = result.SavingsPct
	}

	// Pick the model to send upstream. Router may downgrade to a cheaper
	// model in the same provider family; it never silently upgrades.
	upstreamModel := model
	var overrideModel, overrideReason string
	if p.router != nil {
		decision := p.router.Route(ctx, cfg.name, model, compressedPrompt)
		if p.router.ShouldOverride(model, decision) {
			upstreamModel = decision.Model
			overrideModel = decision.Model
			overrideReason = decision.Reason
		}
	}

	// Circuit breaker override. When the alert manager has tripped a
	// circuit for this (team, feature), force the cheapest model for the
	// provider regardless of what the router decided. The X-Talyvor-Circuit-Open
	// header below tells the client this happened.
	circuitOpen := false
	if p.alertManager != nil && p.alertManager.IsCircuitOpen(team, feature) {
		circuitOpen = true
		upstreamModel = p.alertManager.GetDowngradeModel(cfg.name, model)
	}

	upstreamBodyOut, err := rebuildBody(body, upstreamModel, compressedPrompt)
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(cfg.name, "error").Inc()
		writeError(w, http.StatusBadGateway, "rebuild request body: "+err.Error())
		return
	}

	// forwardWithFallback owns translation, retry, and provider switching.
	// Input is the canonical OpenAI-shape body; output is also OpenAI-shape
	// (Gemini etc. are reverse-translated internally) so all downstream
	// caching / scoring / spend code operates on one schema regardless of
	// which provider actually answered.
	span.AddEvent("llm.forward.start")
	upstreamBody, statusCode, fbResult, err := p.forwardWithFallback(
		ctx, r, cfg.name, upstreamModel, wsID, upstreamBodyOut, w,
	)
	attempts := fbResult.Attempts
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(cfg.name, "error").Inc()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeError(w, http.StatusBadGateway, "upstream LLM error: "+err.Error())
		return
	}

	// Score the response so we can gate caching on quality. Scoring is
	// pure-Go heuristics — fast enough to do on the hot path. Score is
	// only meaningful for a successful upstream (200); on errors we skip
	// scoring entirely.
	var qualityScore *quality.QualityScore
	if p.scorer != nil && statusCode == http.StatusOK {
		q := p.scorer.ScoreResponse(ctx, prompt, string(upstreamBody), cfg.name, model)
		qualityScore = &q
	}

	scoreVal := 0.0
	if qualityScore != nil {
		scoreVal = qualityScore.Score
	}
	span.AddEvent("llm.forward.complete", trace.WithAttributes(
		attribute.Int("lens.attempts", attempts),
		attribute.Float64("lens.quality_score", scoreVal),
	))

	// Headers must be set BEFORE WriteHeader. X-Talyvor-* surface routing
	// decisions to the client; all of these go on the response, never on
	// the upstream request.
	if overrideModel != "" {
		w.Header().Set("X-Talyvor-Model-Override", overrideModel)
	}
	if overrideReason != "" {
		w.Header().Set("X-Talyvor-Route-Reason", overrideReason)
	}
	if circuitOpen {
		w.Header().Set("X-Talyvor-Circuit-Open", "true")
	}
	if qualityScore != nil {
		w.Header().Set("X-Talyvor-Quality-Score", strconv.FormatFloat(qualityScore.Score, 'f', 2, 64))
	}
	if attr.Branch != "" {
		w.Header().Set("X-Talyvor-Branch", attr.Branch)
	}
	if willAttribute {
		w.Header().Set("X-Talyvor-Attributed", "true")
	}
	if attempts > 1 {
		w.Header().Set("X-Talyvor-Attempts", strconv.Itoa(attempts))
	}
	// Record session turn here (BEFORE WriteHeader) so the headers we set
	// next reflect the post-turn totals. Cost is computed against the
	// actually-billed upstream model so it matches the alerts pipeline.
	// LoggingNone skips the turn write entirely (privacy mode); metadata
	// and full both record it.
	if sess != nil && statusCode == http.StatusOK && loggingPolicy != workspace.LoggingNone {
		turnCost := alerts.CostUSD(upstreamModel, len(prompt)/4, len(upstreamBody)/4)
		p.recordSessionTurn(ctx, sessionID, prompt, string(upstreamBody), upstreamModel, turnCost, false)
		setSessionHeaders(w, p, sessionID)
	}
	// forwardWithFallback always returns OpenAI-shape JSON, so we default
	// Content-Type to application/json. Streaming responses are handled in
	// a different code path (stream.go) and don't pass through here.
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(statusCode)
	_, _ = w.Write(upstreamBody)

	if statusCode == http.StatusOK {
		// Cache iff the prompt has no PII AND the response is judged
		// cacheable by the quality scorer. Low-quality responses are
		// forwarded to the client but never persisted.
		shouldCache := !piiDetected
		if qualityScore != nil && !qualityScore.ShouldCache {
			shouldCache = false
		}
		if shouldCache {
			// Cache against the workspace-scoped (uncompressed) prompt +
			// originally requested model so repeat callers in the same
			// workspace get cache hits but other workspaces don't.
			p.storeCaches(ctx, cfg.name, model, cachePrompt, upstreamBody)
		}
		eventPrompt := prompt
		if piiDetected {
			eventPrompt = redactedPrompt
		}
		// Logging policy gates the per-request observability writes. None
		// is the privacy escape hatch — every DB and NATS sink is bypassed.
		// Metadata keeps cost/token rows but strips prompt_text. Full keeps
		// everything (the historic behaviour).
		spendPrompt := eventPrompt
		if loggingPolicy == workspace.LoggingMetadata {
			spendPrompt = ""
		}
		if loggingPolicy == workspace.LoggingFull {
			// Learner publishes to NATS — too verbose for metadata mode.
			p.recordTokenEvent(ctx, cfg.name, model, eventPrompt, upstreamBody, savingsPct, piiDetected)
		}
		// RecordSpend prices the model that was actually billed by the
		// LLM (the upstream model, after any router or circuit override).
		// Fire-and-forget — alert manager failures must never break a
		// successful request.
		inT, outT := len(prompt)/4, len(upstreamBody)/4
		if p.alertManager != nil && loggingPolicy != workspace.LoggingNone {
			// spendPrompt is "" in metadata mode (no prompt text persisted)
			// and the redacted form in full mode when PII was detected.
			if err := p.alertManager.RecordSpend(ctx, team, feature, upstreamModel, inT, outT, spendPrompt, sessionID, requestID); err != nil {
				slog.Warn("alerts: RecordSpend failed",
					slog.String("err", err.Error()),
				)
			}
		}
		// Branch / PR attribution is also best-effort: DB errors here must
		// not propagate to the caller. LoggingNone skips it entirely; the
		// other policies record it. The cost is computed against the
		// upstream model so the same number lands in alerts and attribution.
		if willAttribute && loggingPolicy != workspace.LoggingNone {
			cost := alerts.CostUSD(upstreamModel, inT, outT)
			if err := p.tracker.Record(ctx, attr, upstreamModel, inT, outT, cost); err != nil {
				slog.Warn("attribution: Record failed",
					slog.String("err", err.Error()),
				)
			}
		}
		p.launchABShadows(cfg.name, model, prompt, body)
		metrics.RequestsTotal.WithLabelValues(cfg.name, "forwarded").Inc()
		span.SetAttributes(
			attribute.Bool("lens.cached", false),
			attribute.Float64("lens.cost_usd", alerts.CostUSD(upstreamModel, inT, outT)),
		)
		span.SetStatus(codes.Ok, "")
	} else {
		metrics.RequestsTotal.WithLabelValues(cfg.name, "upstream_error").Inc()
		span.SetStatus(codes.Error, fmt.Sprintf("upstream status %d", statusCode))
	}
}

// rebuildBody re-emits the JSON request body with the (possibly overridden)
// model and the compressed prompt collapsed into a single user message.
// All other fields (temperature, max_tokens, tools, ...) are preserved.
func rebuildBody(originalBody []byte, model, prompt string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(originalBody, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	m["messages"] = []map[string]any{
		{"role": "user", "content": prompt},
	}
	return json.Marshal(m)
}

// tryLocalRouting attempts to serve the request from a locally-hosted
// Ollama model. Returns true if the request was fully handled (response
// written, caches/events updated). Any failure returns false so the
// caller falls through to the regular cloud path.
func (p *Proxy) tryLocalRouting(
	w http.ResponseWriter,
	ctx context.Context,
	provider, model, prompt, cachePrompt, wsID, team, feature, sessionID, requestID string,
	piiDetected bool,
	redactedPrompt string,
) bool {
	if p.localRouter == nil {
		return false
	}
	decision := p.localRouter.ShouldUseLocal(router.AnalyseComplexity(prompt), wsID)
	if !decision.UseLocal {
		return false
	}

	raw, err := p.localRouter.Forward(ctx, decision.Model, prompt)
	if err != nil {
		slog.Warn("localrouter: forward failed, falling through to cloud",
			slog.String("model", decision.Model),
			slog.String("err", err.Error()),
		)
		return false
	}
	formatted, err := p.localRouter.FormatAsOpenAI(raw, decision.Model)
	if err != nil {
		slog.Warn("localrouter: format failed, falling through to cloud",
			slog.String("err", err.Error()),
		)
		return false
	}

	w.Header().Set("X-Talyvor-Local-Model", decision.Model)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(formatted)

	if !piiDetected {
		p.storeCaches(ctx, provider, model, cachePrompt, formatted)
	}
	eventPrompt := prompt
	if piiDetected {
		eventPrompt = redactedPrompt
	}
	// Local runs are free, so the cost recorded by RecordSpend is 0
	// (the model isn't in the price table). recordTokenEvent stores
	// the local model name so usage analytics distinguish local from
	// cloud traffic.
	p.recordTokenEvent(ctx, provider, decision.Model, eventPrompt, formatted, 0, piiDetected)
	if p.alertManager != nil {
		_ = p.alertManager.RecordSpend(ctx, team, feature, decision.Model, len(prompt)/4, len(formatted)/4, eventPrompt, sessionID, requestID)
	}
	metrics.RequestsTotal.WithLabelValues(provider, "local").Inc()
	return true
}

// launchABShadows fans out shadow probes for every active A/B test that
// targets the (provider, requestedModel) pair. Each probe runs in its own
// goroutine against a fresh background context — never blocks the main
// response, never caches the result, never logs prompt content.
func (p *Proxy) launchABShadows(provider, model, prompt string, body []byte) {
	if p.abTester == nil {
		return
	}
	matching := p.abTester.ActiveTestsFor(provider, model)
	for _, test := range matching {
		if !p.abTester.ShouldShadow(test.ID) {
			continue
		}
		apiKey := p.openAIKey
		if test.Provider == "anthropic" {
			apiKey = p.anthropicKey
		}
		testID := test.ID
		// Copy body so a concurrent rebuild upstream can't mutate what
		// the goroutine reads.
		bodyCopy := append([]byte(nil), body...)
		go func() {
			sctx := context.Background()
			result, err := p.abTester.RunShadow(sctx, testID, prompt, bodyCopy, p.httpClient, apiKey)
			if err != nil {
				slog.Warn("ab: shadow probe failed",
					slog.String("test_id", testID),
					slog.String("err", err.Error()),
				)
				return
			}
			if err := p.abTester.RecordResult(sctx, testID, result.Model, *result); err != nil {
				slog.Warn("ab: RecordResult failed",
					slog.String("test_id", testID),
					slog.String("err", err.Error()),
				)
			}
		}()
	}
}

func (p *Proxy) recordTokenEvent(ctx context.Context, provider, model, prompt string, response []byte, savingsPct float64, piiDetected bool) {
	if p.learner == nil {
		return
	}
	// len/4 is the same token approximation the router and compressor use.
	_ = p.learner.Record(ctx, learner.TokenEvent{
		Provider:     provider,
		Model:        model,
		Prompt:       prompt,
		Response:     string(response),
		InputTokens:  len(prompt) / 4,
		OutputTokens: len(response) / 4,
		Cached:       false,
		Compressed:   savingsPct > 0,
		SavingsPct:   savingsPct,
		PIIDetected:  piiDetected,
	})
}

func (p *Proxy) tryExact(ctx context.Context, provider, model, prompt string) []byte {
	if p.exact == nil {
		return nil
	}
	cached, err := p.exact.Get(ctx, provider, model, prompt)
	if err != nil || cached == nil {
		return nil
	}
	return cached
}

func (p *Proxy) trySemantic(ctx context.Context, provider, model, prompt string) []byte {
	if p.semantic == nil {
		return nil
	}
	cached, err := p.semantic.Get(ctx, provider, model, prompt)
	if err != nil || cached == nil {
		return nil
	}
	return cached
}

func (p *Proxy) storeCaches(ctx context.Context, provider, model, prompt string, response []byte) {
	if p.exact != nil {
		_ = p.exact.Set(ctx, provider, model, prompt, response)
	}
	if p.semantic != nil && p.embedder != nil {
		if vec, err := p.embedder.Embed(ctx, prompt); err == nil {
			_ = p.semantic.Set(ctx, provider, model, prompt, response, vec)
		}
	}
}

// forward wraps the upstream call in retry.Do so transient 429/5xx
// responses are retried with exponential backoff before the proxy gives
// up. The closure builds a fresh request each attempt (bytes.NewReader
// keeps the body re-readable). Returns the final response, its body,
// the attempt count, and any non-retryable error.
func (p *Proxy) forward(ctx context.Context, r *http.Request, body []byte, model string, cfg providerConfig) (*http.Response, []byte, int, error) {
	upstreamURL := cfg.upstreamURLFn(model)
	result := retry.Do(ctx, p.retryConfig, func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, http.MethodPost, upstreamURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build upstream request: %w", err)
		}
		for name, values := range r.Header {
			if strings.EqualFold(name, "Host") {
				continue
			}
			for _, v := range values {
				req.Header.Add(name, v)
			}
		}
		cfg.setAuth(req)
		// Inject our current span context as traceparent on the upstream
		// request. If OpenAI / Anthropic ever surface OTel themselves the
		// trace will stay continuous; until then this is harmless metadata.
		otel.GetTextMapPropagator().Inject(c, propagation.HeaderCarrier(req.Header))
		return p.httpClient.Do(req)
	})
	if result.LastError != nil {
		return nil, nil, result.Attempts, result.LastError
	}
	resp := result.Response
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, result.Attempts, fmt.Errorf("read upstream response: %w", err)
	}
	return resp, respBody, result.Attempts, nil
}

func readLimitedBody(r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func extractPrompt(body []byte) (model, prompt string, err error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", "", err
	}

	var sb strings.Builder
	for i, m := range req.Messages {
		if i > 0 {
			sb.WriteByte('\n')
		}
		// Content is usually a string but may be an array of content blocks
		// (Anthropic). For caching purposes, string form is canonical; fall
		// back to raw JSON so block-form prompts still hash deterministically.
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			sb.WriteString(s)
		} else {
			sb.Write(m.Content)
		}
	}
	return req.Model, sb.String(), nil
}

// streamRequested reports whether the JSON body has "stream": true. Parse
// errors fall back to false — the request is treated as non-streaming and
// downstream JSON validation in extractPrompt will surface real malformations.
func streamRequested(body []byte) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	return m.Stream
}

func writeBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// recordSessionTurn is the per-request hook into SessionTracker. It
// keeps the call sites concise: a single helper that captures all the
// per-turn fields without spreading session.Turn construction across
// the proxy. Errors are best-effort — session tracking must never
// break the main response.
func (p *Proxy) recordSessionTurn(ctx context.Context, sessionID, prompt, response, model string, cost float64, cached bool) {
	if p.sessionTracker == nil || sessionID == "" {
		return
	}
	_ = p.sessionTracker.RecordTurn(ctx, sessionID, session.Turn{
		Role:         "user",
		Prompt:       prompt,
		Response:     response,
		Model:        model,
		InputTokens:  len(prompt) / 4,
		OutputTokens: len(response) / 4,
		CostUSD:      cost,
		Cached:       cached,
		CreatedAt:    time.Now().UTC(),
	})
}

// setSessionHeaders stamps the post-turn totals onto the response. Call
// AFTER recordSessionTurn but BEFORE any WriteHeader so the headers are
// part of the committed response.
func setSessionHeaders(w http.ResponseWriter, p *Proxy, sessionID string) {
	if p.sessionTracker == nil || sessionID == "" {
		return
	}
	if s, ok := p.sessionTracker.GetSession(sessionID); ok {
		w.Header().Set("X-Talyvor-Session-Cost", strconv.FormatFloat(s.TotalCostUSD, 'f', 6, 64))
		w.Header().Set("X-Talyvor-Session-Turns", strconv.Itoa(s.TurnCount))
	}
}

// writeJSON is the structured-body equivalent of writeBytes — used by the
// batch dispatch and any other endpoint that wants to emit JSON without
// going through map[string]string.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// withBatchEligibleFlag injects "batch_eligible": true into the body so
// the BatchRouter's IsEligible — which is body-only by signature — can
// see the trigger that was actually carried on the X-Talyvor-Batch HTTP
// header. The downstream Anthropic submit ignores unknown fields, so
// this extra key is harmless when the request does ultimately fly.
func withBatchEligibleFlag(body []byte) []byte {
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

// replayAsSSE re-emits a cached non-streaming response as the provider's
// SSE wire format so strict streaming clients can consume cache hits.
// Frames are computed before any header is committed: on parse failure
// the function returns the error and w is left untouched so the caller
// can fall through cleanly.
func replayAsSSE(w http.ResponseWriter, provider string, cached []byte) error {
	var frames [][]byte
	var err error
	switch provider {
	case "openai":
		frames, err = openAIReplayFrames(cached)
	case "anthropic":
		frames, err = anthropicReplayFrames(cached)
	default:
		return fmt.Errorf("replayAsSSE: unknown provider %q", provider)
	}
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Talyvor-Cache-Replay", "true")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	for _, frame := range frames {
		_, _ = w.Write(frame)
		if flusher != nil {
			flusher.Flush()
		}
	}
	return nil
}

func openAIReplayFrames(cached []byte) ([][]byte, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(cached, &parsed); err != nil {
		return nil, fmt.Errorf("replayAsSSE openai: decode: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("replayAsSSE openai: no choices in cached payload")
	}
	id := "cache-" + uuid.NewString()
	content := parsed.Choices[0].Message.Content

	deltaPayload, err := json.Marshal(map[string]any{
		"id":     id,
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"delta":         map[string]any{"content": content},
			"finish_reason": nil,
		}},
	})
	if err != nil {
		return nil, err
	}
	stopPayload, err := json.Marshal(map[string]any{
		"id":     id,
		"object": "chat.completion.chunk",
		"choices": []map[string]any{{
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
	})
	if err != nil {
		return nil, err
	}
	return [][]byte{
		[]byte("data: " + string(deltaPayload) + "\n\n"),
		[]byte("data: " + string(stopPayload) + "\n\n"),
		[]byte("data: [DONE]\n\n"),
	}, nil
}

func anthropicReplayFrames(cached []byte) ([][]byte, error) {
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(cached, &parsed); err != nil {
		return nil, fmt.Errorf("replayAsSSE anthropic: decode: %w", err)
	}
	if len(parsed.Content) == 0 {
		return nil, fmt.Errorf("replayAsSSE anthropic: no content blocks in cached payload")
	}
	text := parsed.Content[0].Text

	startPayload, _ := json.Marshal(map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	deltaPayload, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	stopPayload, _ := json.Marshal(map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})
	messageStopPayload, _ := json.Marshal(map[string]any{
		"type": "message_stop",
	})

	return [][]byte{
		[]byte("event: content_block_start\ndata: " + string(startPayload) + "\n\n"),
		[]byte("event: content_block_delta\ndata: " + string(deltaPayload) + "\n\n"),
		[]byte("event: content_block_stop\ndata: " + string(stopPayload) + "\n\n"),
		[]byte("event: message_stop\ndata: " + string(messageStopPayload) + "\n\n"),
	}, nil
}

// fallbackAttempt describes one entry in the ordered list of (provider,
// model) pairs forwardWithFallback walks until something succeeds.
type fallbackAttempt struct {
	provider string
	model    string
}

// forwardWithFallback dispatches the request to the original provider
// and, if that attempt is fallback-eligible (5xx, 429, or transport
// error), walks the configured chain trying alternates. Input body is
// canonical OpenAI shape; output is always reverse-translated to that
// same shape so downstream cache + scoring + spend logic doesn't have
// to know which provider actually replied.
func (p *Proxy) forwardWithFallback(
	ctx context.Context,
	r *http.Request,
	provider, model, wsID string,
	body []byte,
	w http.ResponseWriter,
) ([]byte, int, fallback.FallbackResult, error) {
	attempts := []fallbackAttempt{{provider: provider, model: model}}
	if p.fallbackRouter != nil {
		chain := p.fallbackRouter.GetChain(provider)
		// Spec caps total to 3 attempts: original + 2 fallbacks.
		for i, t := range chain {
			if i >= 2 {
				break
			}
			attempts = append(attempts, fallbackAttempt{provider: t.Provider, model: t.Model})
		}
	}

	var (
		lastBody   []byte
		lastStatus int
		lastErr    error
		lastUsed   = attempts[0]
	)

	for i, a := range attempts {
		cfg := p.configForProvider(a.provider)
		if cfg.name == "" {
			// Unknown provider name in the chain — treat as a no-op and move on.
			continue
		}

		// Key selection. When a pool is configured we pick a healthy key
		// per attempt and override cfg's auth/url closures to use it; the
		// per-attempt choice means a fallback retry doesn't reuse a key
		// that just failed. When the pool is empty for this provider we
		// silently fall back to the single configured key.
		var poolKey *keypool.PoolKey
		if p.keyPool != nil {
			if pk, perr := p.keyPool.Get(a.provider); perr == nil && pk != nil {
				cfg = p.applyKey(cfg, pk.Key)
				poolKey = pk
			}
		}

		// Non-original attempts: rewrite the body's model field so the
		// fallback target sees the model it actually supports. The other
		// fields (messages, temperature, tools…) carry through unchanged.
		attemptBody := body
		if i > 0 {
			attemptBody = setModelInBody(body, a.model)
		}

		sendBody := attemptBody
		if cfg.translateRequest != nil {
			translated, terr := cfg.translateRequest(attemptBody)
			if terr != nil {
				if poolKey != nil {
					p.keyPool.RecordError(poolKey.ID)
				}
				lastErr = terr
				lastUsed = a
				continue
			}
			sendBody = translated
		}

		resp, rb, _, ferr := p.forward(ctx, r, sendBody, a.model, cfg)
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}

		// Pool accounting: transport failure is the only signal the spec
		// asks us to count against a key. Upstream 5xx/429s are tracked
		// by the fallback router (which decides whether to switch
		// providers), not against the individual key.
		if poolKey != nil {
			if ferr != nil {
				p.keyPool.RecordError(poolKey.ID)
			} else {
				p.keyPool.RecordSuccess(poolKey.ID)
			}
		}

		if p.fallbackRouter != nil && p.fallbackRouter.ShouldFallback(status, ferr) {
			// Record-and-continue. Logging here is metadata only — no
			// prompt content, no response body.
			slog.Warn("fallback: attempt failed",
				slog.String("original_provider", provider),
				slog.String("attempt_provider", a.provider),
				slog.String("attempt_model", a.model),
				slog.Int("attempt_index", i),
				slog.Int("status", status),
				slog.String("err", errString(ferr)),
				slog.String("workspace_id", wsID),
			)
			lastBody = rb
			lastStatus = status
			lastErr = ferr
			lastUsed = a
			continue
		}

		// Non-fallbackable outcome (success, or 4xx that's the client's
		// fault either way). Reverse-translate Gemini responses so the
		// returned body is OpenAI-shaped for everyone downstream.
		if ferr == nil && cfg.translateResponse != nil && status == http.StatusOK {
			translated, terr := cfg.translateResponse(rb, a.model)
			if terr == nil {
				rb = translated
			}
		}

		result := fallback.FallbackResult{
			UsedProvider: a.provider,
			UsedModel:    a.model,
			Attempts:     i + 1,
			FellBack:     i > 0,
		}
		if i > 0 {
			w.Header().Set("X-Talyvor-Fallback-Provider", a.provider)
			w.Header().Set("X-Talyvor-Fallback-Model", a.model)
			slog.Info("fallback: succeeded",
				slog.String("original_provider", provider),
				slog.String("used_provider", a.provider),
				slog.String("used_model", a.model),
				slog.Int("attempts", i+1),
				slog.String("workspace_id", wsID),
			)
		}
		return rb, status, result, ferr
	}

	// Chain exhausted — return whatever the last attempt produced. The
	// caller's serve() turns a non-nil err into a 502; otherwise the
	// upstream's own 5xx body is forwarded to the client.
	return lastBody, lastStatus, fallback.FallbackResult{
		UsedProvider: lastUsed.provider,
		UsedModel:    lastUsed.model,
		Attempts:     len(attempts),
		FellBack:     len(attempts) > 1,
	}, lastErr
}

// configForProvider returns a providerConfig built fresh per call. The
// closures capture the proxy's URL + key fields so test overrides of
// openAIURL/anthropicURL/googleURL propagate naturally.
func (p *Proxy) configForProvider(name string) providerConfig {
	switch name {
	case "openai":
		return providerConfig{
			name:          "openai",
			upstreamURLFn: func(string) string { return p.openAIURL },
			setAuth: func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+p.openAIKey)
			},
		}
	case "anthropic":
		return providerConfig{
			name:          "anthropic",
			upstreamURLFn: func(string) string { return p.anthropicURL },
			setAuth: func(req *http.Request) {
				req.Header.Set("x-api-key", p.anthropicKey)
				req.Header.Set("anthropic-version", "2023-06-01")
			},
		}
	case "google":
		return providerConfig{
			name: "google",
			upstreamURLFn: func(model string) string {
				return p.googleURL + "/v1beta/models/" + model + ":generateContent?key=" + url.QueryEscape(p.googleKey)
			},
			setAuth: func(*http.Request) {},
			translateRequest: func(body []byte) ([]byte, error) {
				out, _, err := translateToGemini(body)
				return out, err
			},
			translateResponse: translateFromGemini,
		}
	case "bedrock":
		// Snapshot the bedrock state at config-build time so the closures
		// don't race with a concurrent SetBedrockConfig call.
		bedCfg := p.bedrockConfig
		if bedCfg.Region == "" {
			bedCfg.Region = "us-east-1"
		}
		baseURL := p.bedrockURL
		if baseURL == "" {
			baseURL = "https://bedrock-runtime." + bedCfg.Region + ".amazonaws.com"
		}
		return providerConfig{
			name: "bedrock",
			upstreamURLFn: func(model string) string {
				id, ok := modelToBedrockID(model)
				if !ok {
					id = model
				}
				return baseURL + "/model/" + id + "/invoke"
			},
			setAuth: func(req *http.Request) {
				_ = signRequest(req, bedCfg)
			},
			translateRequest:  translateToBedrockFormat,
			translateResponse: translateFromBedrockFormat,
		}
	}
	return providerConfig{}
}

// applyKey returns a providerConfig identical to cfg except that the
// auth and (for Google) URL closures use the supplied key instead of
// the Proxy's single configured value. Used when keyPool.Get returns a
// pooled credential; the original closures stay untouched in cfg's
// source so each call gets a fresh closure capturing the right key.
func (p *Proxy) applyKey(cfg providerConfig, key string) providerConfig {
	switch cfg.name {
	case "openai":
		cfg.setAuth = func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	case "anthropic":
		cfg.setAuth = func(req *http.Request) {
			req.Header.Set("x-api-key", key)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	case "google":
		base := p.googleURL
		cfg.upstreamURLFn = func(model string) string {
			return base + "/v1beta/models/" + model + ":generateContent?key=" + url.QueryEscape(key)
		}
	}
	return cfg
}

// setModelInBody re-emits the JSON body with the model field swapped.
// Parse errors leave the body untouched — forward() will simply send
// the original bytes upstream.
func setModelInBody(body []byte, model string) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = model
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
