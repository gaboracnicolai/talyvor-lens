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

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/audit"
	"github.com/talyvor/lens/internal/backpressure"
	"github.com/talyvor/lens/internal/batch"
	"github.com/talyvor/lens/internal/budget"
	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/keypool"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/localrouter"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/prompts"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/internal/retry"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/routing"
	"github.com/talyvor/lens/internal/session"
	"github.com/talyvor/lens/internal/templates"
	"github.com/talyvor/lens/internal/workspace"
)

const (
	maxBodyBytes                = 4 << 20 // 4 MiB
	openAIChatURL               = "https://api.openai.com/v1/chat/completions"
	anthropicMessageURL         = "https://api.anthropic.com/v1/messages"
	googleGenerativeLanguageURL = "https://generativelanguage.googleapis.com"
	upstreamTimeout             = 120 * time.Second
	defaultWorkspaceID          = "default"
)

// alertSink is the subset of *alerts.AlertManager that proxy.serve()
// touches. Defined locally so tests can drop in a counter mock without
// pulling in the full pgxpool / NATS stack the real manager needs.
type alertSink interface {
	IsCircuitOpen(team, feature string) bool
	GetDowngradeModel(provider, model string) string
	RecordSpend(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool) error
	// RecordSpendWithDistill is RecordSpend plus the token_events.distill_method
	// attribution ("convert" / "vision_ocr"). Used only by the DISTILL request
	// path; non-distilled traffic keeps using RecordSpend.
	RecordSpendWithDistill(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool, distillMethod string) error
}

// budgetGate is the subset of *budgets.Service the proxy hot path touches.
// Defined locally so the proxy can be exercised without a DB-backed
// service. CheckBudget is the spend gate (most-restrictive across the
// workspace/team/sprint budgets); RecordSpend feeds the in-memory running
// totals after a request bills.
type budgetGate interface {
	CheckBudget(ctx context.Context, workspace, team, sprint string, estCost float64) budgets.Decision
	RecordSpend(ctx context.Context, workspace, team, sprint string, cost float64)
}

type Proxy struct {
	exact             *cache.ExactCache
	semantic          *cache.SemanticCache
	embedder          cache.Embedder
	compressor        *compressor.Compressor
	router            *router.Router
	piiDetector       *pii.Detector
	alertManager      alertSink
	templateDetector  *templates.TemplateDetector
	scorer            *quality.Scorer
	tracker           *attribution.Tracker
	attrStore         *attribution.Store
	budgetService     budgetGate
	routingAdvisor    *routing.Advisor
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
	guardrails        *guardrails.Engine
	distiller         *distillIntegration
	poolGate          *cache_pooling.PoolabilityGate
	royaltyMinter     royaltySink

	// Shadow LXC spend (Stage 2.4/2.5) — optional, nil-safe. lxcShadowEnabled
	// is read per-call so the flag stays live. See shadow_lxc.go.
	lxcSink          lxcSpendSink
	lxcShadowEnabled func() bool

	// LXC gating (Stage 2.4/2.5) — optional, nil-safe pre-serve block.
	// Inert unless lxcGatingEnabled() AND lxcShadowEnabled() (see lxc_gate.go).
	lxcGate          lxcBalanceReader
	lxcGatingEnabled func() bool

	// Routing-pattern capture (Phase-3) — optional, nil-safe post-serve
	// producer for the routing Advisor. See pattern_capture.go.
	patternSink           patternCaptureSink
	patternCaptureEnabled func() bool

	// WorkTier descriptive classifier (optional, nil-safe post-serve). Shares the
	// obsLimiter budget with pattern capture. See worktier_capture.go.
	workTierSink    workTierSink
	workTierEnabled func() bool

	// obsLimiter bounds post-serve observational writes (pattern capture).
	// nil = no bound. main wires the same limiter into attribution's
	// RecordAsync so the total observational claim on the DB pool stays
	// bounded (#122). See pattern_capture.go / internal/backpressure.
	obsLimiter *backpressure.Limiter

	// Distill attribution (S1) — optional, nil-safe post-serve, MINT-FREE
	// record of a consented cross-tenant pooled-distill serve. See
	// distill_attribution.go. nil ⇒ attribution off (inert).
	distillAttribSink distillAttributionSink

	// Routing-pattern EARNING (S4) — optional, nil-safe. Separate sink from
	// capture (this one can mint via RecordPattern). See pattern_earn.go.
	patternEarnSink    patternEarnSink
	patternEarnEnabled func() bool
	retryConfig        retry.Config
	httpClient         *http.Client
	openAIKey          string
	anthropicKey       string
	googleKey          string
	learner            *learner.Learner

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

	// Extra OpenAI-compatible providers (Mistral, Groq, vLLM). Keys
	// land here via SetExtraProviderConfig; URLs default to the real
	// endpoints, tests override. vLLM has no public default — the
	// operator runs the inference server somewhere.
	mistralKey string
	groqKey    string
	vllmKey    string
	mistralURL string
	groqURL    string
	vllmURL    string
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
	guardrailsEngine *guardrails.Engine,
	openAIKey string,
	anthropicKey string,
	googleKey string,
	learners ...*learner.Learner,
) *Proxy {
	p := &Proxy{
		exact:             exactCache,
		semantic:          semanticCache,
		embedder:          embedder,
		compressor:        compressorImpl,
		router:            routerImpl,
		piiDetector:       piiDetector,
		templateDetector:  templateDetector,
		scorer:            scorer,
		tracker:           tracker,
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
		guardrails:        guardrailsEngine,
		retryConfig:       retry.DefaultConfig(),
		httpClient:        &http.Client{Timeout: upstreamTimeout},
		openAIKey:         openAIKey,
		anthropicKey:      anthropicKey,
		googleKey:         googleKey,
		openAIURL:         openAIChatURL,
		anthropicURL:      anthropicMessageURL,
		googleURL:         googleGenerativeLanguageURL,
		mistralURL:        "https://api.mistral.ai",
		groqURL:           "https://api.groq.com",
		// vllmURL stays empty by default — operator-supplied.
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
// SetAttributionStore wires the upgraded per-request
// attribution store (Upgrade Batch 1 / Item 3). Keeping this
// as a setter rather than a constructor arg avoids re-shuffling
// the already-long proxy.New signature; main.go calls it right
// after constructing the proxy.
func (p *Proxy) SetAttributionStore(s *attribution.Store) {
	p.attrStore = s
}

// SetRoutingAdvisor wires the pattern-network routing advisor (Upgrade 22).
// A setter so proxy.New's signature stays put. A nil advisor (or one whose
// Enabled() is false) leaves model selection byte-for-byte unchanged.
func (p *Proxy) SetRoutingAdvisor(a *routing.Advisor) {
	if a != nil {
		p.routingAdvisor = a
	}
}

// SetBudgetService wires the per-team / per-sprint budget governor
// (Upgrade 19). A setter, like SetAttributionStore, so proxy.New's
// signature stays put. A nil service disables the budget gate entirely —
// the workspace spend cap continues to enforce on its own.
func (p *Proxy) SetBudgetService(s *budgets.Service) {
	// Guard the typed-nil interface trap so `p.budgetService != nil` holds.
	if s != nil {
		p.budgetService = s
	}
}

func (p *Proxy) setAlertSink(sink alertSink) {
	p.alertManager = sink
}

// SetPoolGate enables the Phase-2 Stage 2.0 shared-cache governance gate. Wired
// as a setter so proxy.New's signature stays put. When unset (nil), the pooled
// path is fully inert: storeCaches still owner-tags private entries but writes no
// pooled copy, and the request path never attempts a cross-tenant read.
func (p *Proxy) SetPoolGate(gate *cache_pooling.PoolabilityGate) {
	p.poolGate = gate
}

// royaltySink is the Phase-2 Stage 2.1 Pool-B royalty surface: one call per
// SERVED cross-tenant pooled hit. *poolroyalty.Minter satisfies it; exactly-
// once (the request_id claim) and the inert-by-default flag live in the
// Minter, not here — the proxy only reports what it actually served.
type royaltySink interface {
	MintServedHit(ctx context.Context, h poolroyalty.ServedHit) (poolroyalty.Result, error)
}

// SetRoyaltyMinter enables the Stage 2.1 Pool-B royalty mint. Wired as a
// setter so proxy.New's signature stays put. When unset (nil), pooled hits
// serve exactly as Stage 2.0 left them and nothing mints.
func (p *Proxy) SetRoyaltyMinter(m royaltySink) {
	p.royaltyMinter = m
}

// extractResponseContent pulls the assistant text out of an OpenAI-shape
// response (forwardWithFallback normalizes every provider to this shape), so
// the output guardrails inspect the model's actual output, not the JSON
// envelope. Returns "" when the shape doesn't match.
func extractResponseContent(body []byte) string {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &r) == nil && len(r.Choices) > 0 {
		return r.Choices[0].Message.Content
	}
	return ""
}

// replaceResponseContent rewrites the first choice's message content (used to
// re-inject redacted output). Returns the body unchanged when the shape
// doesn't match, so a redaction can never corrupt the response envelope.
func replaceResponseContent(body []byte, newContent string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	choices, ok := m["choices"].([]any)
	if !ok || len(choices) == 0 {
		return body
	}
	c0, ok := choices[0].(map[string]any)
	if !ok {
		return body
	}
	msg, ok := c0["message"].(map[string]any)
	if !ok {
		return body
	}
	msg["content"] = newContent
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// isAutoRoute reports whether a request cedes its model choice to the
// routing advisor — either the "auto" pseudo-model (a convention developers
// recognise from model-router gateways) or an explicit X-Talyvor-Auto-Route
// header. Any concrete model name is treated as pinned and always honored.
func isAutoRoute(r *http.Request, model string) bool {
	if strings.EqualFold(strings.TrimSpace(model), "auto") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("X-Talyvor-Auto-Route"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
	// requestStart is captured before any work so the per-request
	// attribution row can report wall-clock latency for the IDE
	// dashboard. The legacy alerts pipeline doesn't use it; this
	// is a parallel signal recorded asynchronously after the
	// response finishes.
	requestStart := time.Now()

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

	// Modality detection (Upgrade 15). Cheap + structural — inspects content
	// block types only, never decodes the base64 image/audio bytes. Drives
	// capability-aware routing below and the modality dimension on the spend
	// record. Always recorded so the dashboard sees the request mix.
	modSet := modality.Detect(body)
	metrics.RequestByModality(modSet.Label())
	w.Header().Set("X-Talyvor-Modality", modSet.Label())

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

	// DISTILL request-path integration (Upgrade 23). When the workspace + this
	// request opt in AND a document block is present, convert it to clean
	// Markdown via the KILLABLE subprocess before the model sees it. This runs
	// after the workspace gate (so a blocked workspace spends no conversion) and
	// before guardrails/cache/capability (so they all operate on the distilled
	// text). INERT-BY-DEFAULT: a non-opted-in request, a request with no
	// document, or a conversion failure leaves body/prompt/modSet byte-for-byte
	// unchanged. After a successful distill we re-derive the variables computed
	// from the body — prompt, modSet (now text-only, so the capability gate
	// won't redirect to a vision model), and the workspace-scoped cachePrompt.
	// distillMethod tags the spend row written far below ("" = not distilled);
	// visionOCR carries any OCR sub-call cost to book as its own spend row.
	distillMethod := ""
	var visionOCR visionSpend
	var distillFacts []distillServeFact // S1: consented cross-tenant serves, recorded post-flush below
	if p.distiller != nil {
		// The live vision-OCR dispatcher for this request (same provider, scoped
		// to the workspace allow-list). On a text-less document the orchestrator
		// uses it to recover text via a vision model and books the cost honestly;
		// a nil-safe failure path leaves a NeedsVision document untouched.
		vd := p.newVisionDispatcher(r, cfg, wsID)
		if nb, np, nm, did, vs, dfacts := p.distiller.MaybeDistill(ctx, r, body, wsID, modSet, vd); did {
			body, prompt, modSet = nb, np, nm
			distillFacts = dfacts
			cachePrompt = prompt
			if p.workspaceManager != nil {
				cachePrompt = wsID + ":" + prompt
			}
			// Durable DISTILL attribution for the spend write below: the main
			// (lower-count) row is tagged 'convert' (the saving is implicit in the
			// reduced count — never a second write), and any OCR sub-call cost is
			// booked as its OWN 'vision_ocr' row, never blended.
			distillMethod = "convert"
			visionOCR = vs
			// modSet is now the post-distill (text-only) set, so the capability
			// gate + spend below bill the converted text. The X-Talyvor-Modality
			// header + RequestByModality metric above intentionally keep the
			// INCOMING modality (what the client sent) — that's the request-mix
			// signal, and the X-Talyvor-Distill header below tells the client we
			// converted it.
			w.Header().Set("X-Talyvor-Distill", "applied")
		}
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
	// Sprint follows the issue convention — a client-supplied header, not
	// derived from the API key. Empty when the caller isn't tracking sprints.
	sprint := r.Header.Get("X-Talyvor-Sprint")

	// Budget gate (Upgrade 19). Sits alongside the workspace spend cap but
	// governs workspace / team / sprint budgets with most-restrictive-wins.
	// Reads the in-memory budget snapshot only — no per-request DB hit. A
	// hard_block budget already over its limit rejects with 402; alert and
	// off budgets never reject here (they notify on the recording path). The
	// estimate uses input tokens only (output is unknown pre-call), so the
	// gate under- rather than over-blocks; the true cost is booked later.
	if p.budgetService != nil {
		estCost := alerts.CostUSD(model, len(prompt)/4, 0)
		if p.budgetService.CheckBudget(ctx, wsID, team, sprint, estCost) == budgets.DecisionBlock {
			writeError(w, http.StatusPaymentRequired, "budget exceeded for workspace/team/sprint")
			metrics.RequestsTotal.WithLabelValues(cfg.name, "budget_blocked").Inc()
			return
		}
	}

	// LXC gating (Stage 2.4/2.5) — pre-serve block when the workspace can't
	// afford the estimated LXC cost. Sits alongside the budget gate, BEFORE the
	// upstream call. Inert unless LXCGatingEnabled AND shadow are both on; the
	// estimate is input-only (under-blocks); a balance-read error fails open.
	if p.lxcGateBlocks(ctx, wsID, model, prompt, loggingPolicy) {
		writeError(w, http.StatusPaymentRequired, "insufficient LXC balance for estimated request cost")
		metrics.RequestsTotal.WithLabelValues(cfg.name, "lxc_blocked").Inc()
		return
	}

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

	// Guardrails pipeline. One call subsumes the previous PII + injection
	// blocks plus topic / word filter / custom-regex rules. Block-action
	// violations short-circuit with a 400; redact-action results rewrite
	// the prompt going forward. piiDetected is preserved as a downstream
	// signal so the cache layer still refuses to persist PII responses.
	piiDetected := false
	guardrailFired := false // WorkTier sensitivity cause B (a non-blocking guardrail tripped)
	var redactedPrompt string
	if p.guardrails != nil {
		gr := p.guardrails.Check(ctx, wsID, prompt, body)
		w.Header().Set("X-Talyvor-Risk-Score", strconv.FormatFloat(gr.RiskScore, 'f', 2, 64))
		// Per-type/action guardrail metrics (Upgrade 13). Bounded labels.
		for _, v := range gr.Violations {
			metrics.GuardrailTriggered(v.Type, string(v.Action))
			if v.Action == guardrails.ActionRedact {
				metrics.GuardrailRedaction(v.Type)
			}
		}
		if !gr.Passed {
			blockType := "guardrail"
			for _, v := range gr.Violations {
				if v.Action == guardrails.ActionBlock {
					blockType = v.Type
				}
			}
			metrics.GuardrailBlock(blockType)
			slog.Warn("proxy: guardrail blocked",
				slog.String("provider", cfg.name),
				slog.String("workspace_id", wsID),
				slog.Float64("risk_score", gr.RiskScore),
				slog.Int("violation_count", len(gr.Violations)),
			)
			metrics.RequestsTotal.WithLabelValues(cfg.name, "guardrail_blocked").Inc()
			w.Header().Set("X-Talyvor-Guardrail-Blocked", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":      "guardrail violation",
				"violations": gr.Violations,
				"risk_score": gr.RiskScore,
			})
			return
		}
		for _, v := range gr.Violations {
			if v.Type == "pii" {
				piiDetected = true
				w.Header().Set("X-Talyvor-PII-Detected", "true")
			}
		}
		if gr.RedactedPrompt != "" && gr.RedactedPrompt != prompt {
			redactedPrompt = gr.RedactedPrompt
			w.Header().Set("X-Talyvor-Guardrail-Redacted", "true")
		}
		// WorkTier sensitivity input: a guardrail FIRED (redact/warn) on a served
		// request. A blocked request returned above, so this only marks non-block
		// fires. Carried, NON-CONTENT (a bool, never the matched span).
		guardrailFired = len(gr.Violations) > 0
		if len(gr.Violations) > 0 && piiDetected {
			metrics.RequestsTotal.WithLabelValues(cfg.name, "pii_skip_cache").Inc()
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
		// pooledHit is non-nil ONLY when the response came from the shared
		// pool — Stage 2.1 royalty attribution, captured at lookup but
		// consumed exclusively at the SERVE points below. A found-but-not-
		// served hit (e.g. SSE replay failure falling through to the live
		// LLM) therefore reports nothing and mints nothing.
		var pooledHit *poolroyalty.ServedHit
		span.AddEvent("cache.check.exact")
		if c := p.tryExact(ctx, cfg.name, model, cachePrompt); c != nil {
			cached, layer = c, "cache_hit_exact"
			span.AddEvent("cache.hit.exact")
		} else {
			span.AddEvent("cache.check.semantic")
			if c := p.trySemantic(ctx, cfg.name, model, cachePrompt, wsID); c != nil {
				cached, layer = c, "cache_hit_semantic"
				span.AddEvent("cache.hit.semantic")
			} else if p.poolGate.Participant(wsID) {
				// Private miss + this workspace opted into pooling: a poolable
				// requester may be served an entry CONTRIBUTED by another poolable
				// workspace, found under the un-prefixed pooled key. The serve
				// requires the contributor to ALSO be opted in (verified via the
				// owner stamped on the entry) — gated by MaybeAllowPooledHit, which
				// needs global + requester + contributor all true. Inert by
				// default: Participant is false when the gate is nil/off, so this
				// whole branch (and its extra cache read) never runs.
				if c, owner := p.tryExactPooled(ctx, cfg.name, model, prompt); c != nil && p.poolGate.MaybeAllowPooledHit(ctx, wsID, owner) {
					cached, layer = c, "cache_hit_pooled"
					pooledHit = &poolroyalty.ServedHit{
						RequestID:            requestID,
						RequesterWorkspace:   wsID,
						ContributorWorkspace: owner,
						Layer:                "exact",
						EntryID:              p.exact.Key(cfg.name, model, pooledPromptKey(prompt)),
						Provider:             cfg.name,
						Model:                model,
					}
					span.AddEvent("cache.hit.pooled")
				} else if c, owner, entryID, sim := p.trySemanticPooled(ctx, cfg.name, model, prompt); c != nil && p.poolGate.MaybeAllowPooledHit(ctx, wsID, owner) {
					cached, layer = c, "cache_hit_pooled_semantic"
					pooledHit = &poolroyalty.ServedHit{
						RequestID:            requestID,
						RequesterWorkspace:   wsID,
						ContributorWorkspace: owner,
						Layer:                "semantic",
						EntryID:              entryID,
						Provider:             cfg.name,
						Model:                model,
						Similarity:           sim,
					}
					span.AddEvent("cache.hit.pooled_semantic")
				} else {
					metrics.RecordCacheMiss("cache_miss")
				}
			} else {
				// Both exact and semantic missed — a true cache miss. Symmetric
				// with the RecordCacheHit calls below.
				metrics.RecordCacheMiss("cache_miss")
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
					metrics.RecordCacheHit(layer)
					// SERVE point: the replay succeeded, so a pooled hit
					// (if that's what this was) now earns its royalty.
					p.mintPooledRoyalty(ctx, pooledHit, prompt, cached, loggingPolicy)
					span.SetAttributes(
						attribute.Bool("lens.cached", true),
						attribute.Float64("lens.cost_usd", 0),
					)
					span.SetStatus(codes.Ok, "")
					return
				}
				// NOT served: fall through to the live LLM below — the
				// pooled hit (if any) must NOT mint (claim at serve, not
				// at lookup).
				slog.Warn("proxy: cached payload not replayable as SSE; falling through to LLM",
					slog.String("provider", cfg.name),
				)
			} else {
				writeBytes(w, http.StatusOK, cached)
				metrics.RequestsTotal.WithLabelValues(cfg.name, layer).Inc()
				metrics.RecordCacheHit(layer)
				// SERVE point: the cached body went out on the wire.
				p.mintPooledRoyalty(ctx, pooledHit, prompt, cached, loggingPolicy)
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
	// routing must never break the main request. Multimodal requests
	// skip local entirely (the local text models can't serve images) and
	// fall through to the capability-aware cloud path below.
	if !modSet.Multimodal() && p.tryLocalRouting(w, ctx, cfg.name, model, prompt, cachePrompt, wsID, team, sprint, feature, sessionID, requestID, piiDetected, redactedPrompt) {
		return
	}

	// Streaming path: detected by "stream": true in the request JSON. The
	// stream handler forwards SSE chunks unbuffered, then caches the
	// assembled response after the upstream stream completes. We skip the
	// compression + routing path for streams since that would rewrite the
	// body and break wire-compatibility with the live SSE.
	// Streaming skips the routing/capability path below (to preserve SSE
	// wire-compatibility — it must not rewrite the body), so the capability
	// gate is enforced here: a multimodal stream to a model that can't serve
	// the modality fails fast rather than streaming a wrong answer.
	if streaming && modSet.Multimodal() && !modality.Supports(model, modSet) {
		metrics.ModalityUnsupported()
		writeError(w, http.StatusUnprocessableEntity,
			"streaming request contains "+modSet.Label()+" content but model "+model+" does not support it")
		return
	}
	// Output guardrails (Upgrade 13) can't inspect an in-flight stream. By
	// default they're not applied to streams (marked on the response); a
	// workspace can opt into buffering — which gives up streaming so the full
	// response can be inspected — by trading the SSE fast-path for the
	// buffered non-streaming path below.
	bufferedStream := false
	if streaming && p.guardrails != nil && p.guardrails.ShouldBufferStream(wsID) {
		w.Header().Set("X-Talyvor-Stream-Buffered", "true")
		streaming = false
		bufferedStream = true
	}
	if streaming {
		if p.guardrails != nil && p.guardrails.OutputEnabled() {
			w.Header().Set("X-Talyvor-Output-Guardrails", "not-applied-streaming")
		}
		sh := &StreamHandler{proxy: p}
		// Streaming skips routing, so the billed model is the requested
		// model. The fallback input estimate mirrors the non-streaming path:
		// modality-aware for multimodal, else len(prompt)/4.
		estIn := len(prompt) / 4
		if modSet.Multimodal() {
			estIn = modSet.EstimateInputTokens()
		}
		sc := streamSpend{
			wsID: wsID, team: team, sprint: sprint, feature: feature,
			model: model, requestID: requestID, sessionID: sessionID,
			modality: modSet.Label(), logging: loggingPolicy, estInputTokens: estIn,
		}
		var serr error
		if cfg.name == "openai" {
			serr = sh.ServeOpenAI(w, r, cfg.name, model, prompt, cachePrompt, body, piiDetected, sc)
		} else {
			serr = sh.ServeAnthropic(w, r, cfg.name, model, prompt, cachePrompt, body, piiDetected, sc)
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

	// Routing intelligence (Upgrade 22) engages ONLY when enabled AND the
	// request explicitly cedes the model choice ("auto" pseudo-model or the
	// X-Talyvor-Auto-Route header). A concrete model is PINNED and falls
	// through to the existing router path below — byte-for-byte unchanged.
	if p.routingAdvisor.Enabled() && isAutoRoute(r, model) {
		var allowedModels, allowedProviders []string
		if ws, ok := p.workspaceManager.GetWorkspace(wsID); ok {
			allowedModels, allowedProviders = ws.AllowedModels, ws.AllowedProviders
		}
		// In-memory lookup only — never a DB query on the request path.
		rec := p.routingAdvisor.Recommend(ctx, wsID, feature, len(compressedPrompt)/4, cfg.name, allowedModels, allowedProviders)
		if rec.Basis != routing.BasisNone && rec.Model != "" {
			upstreamModel = rec.Model
			overrideModel = rec.Model
			overrideReason = rec.Reason
			metrics.RoutingIntelligenceApplied()
		} else if p.router != nil {
			// No qualifying recommendation — fall back to the complexity
			// router so an "auto" request still gets a concrete model.
			decision := p.router.Route(ctx, cfg.name, model, compressedPrompt)
			if decision.Model != "" {
				upstreamModel = decision.Model
				overrideModel = decision.Model
				overrideReason = "routing default (no qualifying intelligence): " + decision.Reason
			}
			metrics.RoutingFallback()
		} else {
			metrics.RoutingFallback()
		}
	} else if p.router != nil {
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

	// Capability hard constraint (Upgrade 15). A multimodal request MUST go
	// to a model that can serve the modality — unlike routing intelligence
	// (advisory), this is a hard gate on the FINAL model, and we never
	// silently strip the content and send text-only:
	//   - auto-route + incapable → redirect to the cheapest capable allowed
	//     model of this provider; if none → fail fast 422.
	//   - pinned + incapable → fail fast 422 (never silently serve an image
	//     at a model the caller explicitly pinned that can't see it).
	if modSet.Multimodal() && !modality.Supports(upstreamModel, modSet) {
		var allowedModels []string
		if ws, ok := p.workspaceManager.GetWorkspace(wsID); ok {
			allowedModels = ws.AllowedModels
		}
		if isAutoRoute(r, model) {
			capable, ok := modality.CapableModel(cfg.name, modSet, allowedModels)
			if !ok {
				metrics.ModalityUnsupported()
				writeError(w, http.StatusUnprocessableEntity,
					"request contains "+modSet.Label()+" content but no configured "+cfg.name+" model supports it")
				return
			}
			w.Header().Set("X-Talyvor-Vision-Redirect", upstreamModel+"→"+capable)
			upstreamModel = capable
			overrideModel = capable
			overrideReason = "modality redirect: " + modSet.Label() + " requires a capable model"
			metrics.VisionRouteRedirect()
		} else {
			metrics.ModalityUnsupported()
			writeError(w, http.StatusUnprocessableEntity,
				"request contains "+modSet.Label()+" content but the requested model "+model+" does not support it")
			return
		}
	}

	upstreamBodyOut, err := rebuildBody(body, upstreamModel, compressedPrompt)
	if err == nil && bufferedStream {
		// Buffering for output guardrails: force the UPSTREAM call non-streaming
		// so it returns a parseable completion CheckOutput can inspect (the
		// client still gets a stream-shaped response below). Every OpenAI-
		// compatible provider — including Anthropic — reads this top-level
		// "stream" field verbatim; Google already uses the non-streaming
		// endpoint, so a single override covers all providers.
		upstreamBodyOut = disableStream(upstreamBodyOut)
	}
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

	// Output guardrails (Upgrade 13) — full, non-streaming response. Off by
	// default (CheckOutput no-ops when disabled → behaves as today). On block
	// we write a 422 + reject the content but still record spend (the upstream
	// call already ran); on redact we re-inject the masked content so the
	// client, cache, and spend record all see the masked response.
	clientBody := upstreamBody
	clientStatus := statusCode
	outputBlocked := false
	if statusCode == http.StatusOK && p.guardrails != nil && p.guardrails.OutputEnabled() {
		w.Header().Set("X-Talyvor-Output-Guardrails", "applied")
		ogr := p.guardrails.CheckOutput(ctx, wsID, extractResponseContent(upstreamBody))
		for _, v := range ogr.Violations {
			metrics.GuardrailTriggered(v.Type, string(v.Action))
		}
		switch {
		case !ogr.Passed:
			outputBlocked = true
			blockType := "output"
			for _, v := range ogr.Violations {
				if v.Action == guardrails.ActionBlock {
					blockType = v.Type
				}
			}
			metrics.GuardrailBlock(blockType)
			w.Header().Set("X-Talyvor-Output-Guardrail-Blocked", "true")
			clientStatus = http.StatusUnprocessableEntity
			clientBody, _ = json.Marshal(map[string]any{
				"error":      "output guardrail violation",
				"violations": ogr.Violations,
			})
		case ogr.RedactedPrompt != "":
			upstreamBody = replaceResponseContent(upstreamBody, ogr.RedactedPrompt)
			clientBody = upstreamBody
			metrics.GuardrailRedaction("output_pii")
			w.Header().Set("X-Talyvor-Output-Redacted", "true")
		}
	}

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
	if bufferedStream && clientStatus == http.StatusOK {
		// The client asked for a stream; we buffered the full response so the
		// output guardrails could inspect it. Deliver the (possibly redacted)
		// result as a single SSE event so the client still gets a stream-shaped
		// response — just not incrementally. A blocked output falls through to
		// the JSON 422 below (errors aren't streamed).
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(clientStatus)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(clientBody)
		_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
	} else {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(clientStatus)
		_, _ = w.Write(clientBody)
	}

	// A blocked output is treated like a blocked request: no caching, no
	// spend, no attribution. The guardrail block itself is recorded
	// (metric + log + the 422 response); the workspace isn't billed for a
	// response we refused to return.
	if statusCode == http.StatusOK && !outputBlocked {
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
			// workspace get cache hits but other workspaces don't. The raw
			// prompt + wsID also feed the opt-in pooled (cross-tenant) write.
			p.storeCaches(ctx, cfg.name, model, cachePrompt, prompt, wsID, upstreamBody)
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
		// Bill on the provider's REPORTED usage when it surfaces one — that
		// is the exact count the provider charged (and for multimodal it
		// already folds in the image cost, beating the flat per-image
		// estimate). Fall back to a len/4 estimate only when usage is absent,
		// and mark the row estimated HONESTLY so budgets/ROI know which rows
		// are real. For multimodal the fallback uses a modality-aware figure
		// (text chars + per-image) rather than len(prompt)/4, which would
		// count the flattened base64 blob.
		inT, outT := len(prompt)/4, len(upstreamBody)/4
		costEstimated := true
		spendSource := "estimated"
		if u, ok := cfg.ExtractUsage(upstreamBody); ok {
			inT, outT = u.InputTokens, u.OutputTokens
			costEstimated = false
			spendSource = "provider_usage"
		} else if modSet.Multimodal() {
			inT = modSet.EstimateInputTokens()
		}
		if p.alertManager != nil && loggingPolicy != workspace.LoggingNone {
			// spendPrompt is "" in metadata mode (no prompt text persisted)
			// and the redacted form in full mode when PII was detected.
			// wsID + sprint travel on this single billing write so spend is
			// attributable per workspace / team / sprint (see migration 0028);
			// modality + estimated ride along too (migration 0029). distillMethod
			// rides along too (migration 0040): "convert" for a distilled request
			// (the saving is IMPLICIT in this row's lower count), "" otherwise.
			metrics.SpendRecord(spendSource)
			var recErr error
			if distillMethod != "" {
				recErr = p.alertManager.RecordSpendWithDistill(ctx, wsID, team, sprint, feature, upstreamModel, inT, outT, spendPrompt, sessionID, requestID, modSet.Label(), costEstimated, distillMethod)
			} else {
				recErr = p.alertManager.RecordSpend(ctx, wsID, team, sprint, feature, upstreamModel, inT, outT, spendPrompt, sessionID, requestID, modSet.Label(), costEstimated)
			}
			if recErr != nil {
				slog.Warn("alerts: RecordSpend failed",
					slog.String("err", recErr.Error()),
				)
			}
			// Shadow LXC debit — INSIDE the logging gate, alongside the durable
			// cost_usd write, so it fires iff that write fires (a LoggingNone
			// workspace gets neither; symmetric with the streaming seam, which
			// returns early on LoggingNone). Void, post-serve, same ctx — cannot
			// affect the response. Inert unless the flag is on AND a sink wired.
			p.shadowSpendLXC(ctx, wsID, alerts.CostUSD(upstreamModel, inT, outT))
			// Routing-pattern capture (Phase-3) — post-serve, VOID, structurally
			// mint-free. Same logging gate + post-serve position as the shadow
			// debit (a LoggingNone workspace gets neither); the opt-in WRITE gate
			// is in the sink SQL. cacheHit=false: this is the upstream model-call
			// path (cache hits short-circuit far earlier). Quality is the
			// just-scored value; latency is the real request elapsed. Streaming
			// is deliberately NOT captured (no scored quality on that path —
			// see pattern_capture.go).
			// scored = we actually computed a quality score (scorer wired AND
			// statusCode==200); an unscored/non-200 response must NOT write a
			// quality=0 row that poisons the Advisor's averages.
			// Routing-pattern earn-or-capture (S4) — mutually exclusive, ONE row.
			// earnPattern returns true ONLY when it took the corpus row (flag on
			// AND authenticated non-default workspace AND opted-in); in every
			// other state (incl. flag-OFF, the first guard) it returns false and
			// capturePattern runs byte-identical to before. Both are post-flush,
			// detached, void — neither can affect the served response.
			if !p.earnPattern(ctx, feature, upstreamModel, cfg.name, prompt, upstreamBody,
				len(compressedPrompt)/4, outT, scoreVal, qualityScore != nil, time.Since(requestStart).Milliseconds()) {
				p.capturePattern(ctx, wsID, feature, upstreamModel, cfg.name,
					len(compressedPrompt)/4, outT, scoreVal, qualityScore != nil, time.Since(requestStart).Milliseconds(), false)
			}
			// WorkTier descriptive classification — post-flush, off-hot-path, void,
			// best-effort, shares the obsLimiter; default-off. DESCRIPTIVE + mint-free
			// (the sink has no ledger handle). Complexity is derived on the SAME input
			// the router analyzed (compressedPrompt) so it equals the routing decision's.
			p.captureWorkTier(ctx, wsID, feature, upstreamModel, cfg.name, compressedPrompt,
				len(compressedPrompt)/4, outT, piiDetected, guardrailFired, string(loggingPolicy))
			// S1 distill attribution (MINT-FREE) — record any consented
			// cross-tenant pooled-distill serves surfaced from MaybeDistill.
			// Post-flush, void, detached, swallowed (mirrors capturePattern);
			// self-serve already skipped upstream, LoggingNone suppressed in the
			// sink. Inert unless an attribution sink is wired.
			p.recordDistillServes(ctx, wsID, loggingPolicy, distillFacts)
			// The vision-OCR sub-call's cost is its OWN row, tagged 'vision_ocr',
			// priced on the vision model, flagged estimated — a COST, never a
			// saving, NEVER blended into the 'convert' row above. The durable
			// monthly spend cap (SUM(cost_usd) over token_events) then includes it.
			if visionOCR.recorded() {
				// The OCR row is a cost_estimated row (document/image token
				// accounting is approximate), so keep the SpendRecord source label
				// within its bounded domain (provider_usage|estimated).
				metrics.SpendRecord("estimated")
				// A successful OCR must name its model so the cost prices (an empty
				// model → cost_usd=0, unbudgeted). Production always sets it; warn
				// loudly if a dispatcher ever doesn't, rather than silently $0.
				if visionOCR.model == "" {
					slog.Warn("distill: vision-OCR cost recorded WITHOUT a model — it cannot be priced (cost_usd=0)",
						slog.String("workspace_id", wsID),
						slog.Int("ocr_input_tokens", visionOCR.inputTokens),
						slog.Int("ocr_output_tokens", visionOCR.outputTokens),
					)
				}
				if err := p.alertManager.RecordSpendWithDistill(ctx, wsID, team, sprint, feature, visionOCR.model, visionOCR.inputTokens, visionOCR.outputTokens, "", sessionID, requestID, "document", true, "vision_ocr"); err != nil {
					slog.Warn("alerts: vision-OCR RecordSpend failed",
						slog.String("err", err.Error()),
					)
				}
			}
		}
		// Feed the in-memory budget totals from the SAME billed cost. This is
		// a memory update (+ threshold checks), not a second hot-path DB
		// write — token_events above is the durable record.
		if p.budgetService != nil {
			p.budgetService.RecordSpend(ctx, wsID, team, sprint, alerts.CostUSD(upstreamModel, inT, outT))
		}
		// (The legacy branch_spend double-write was retired in #157 — it had no
		// reader since #158. request_attribution below is the sole attribution
		// write now; attr/willAttribute still drive the X-Talyvor-Branch echo.)
		// Upgraded per-request attribution. Always fired (the
		// store handles the empty-workspace case by skipping the
		// insert) and always async so a slow Postgres can't slow
		// the response. Cost + token figures are the same numbers
		// the alerts pipeline records, keeping the dashboard
		// reconciliation honest.
		if p.attrStore != nil && loggingPolicy != workspace.LoggingNone {
			cost := alerts.CostUSD(upstreamModel, inT, outT)
			p.attrStore.RecordAsync(
				attribution.ExtractFromRequest(r),
				inT, outT, cost,
				upstreamModel, cfg.name,
				time.Since(requestStart),
			)
		}
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
	provider, model, prompt, cachePrompt, wsID, team, sprint, feature, sessionID, requestID string,
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
		p.storeCaches(ctx, provider, model, cachePrompt, prompt, wsID, formatted)
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
		// Local routing is text-only (multimodal skips it upstream). The token
		// counts here are ESTIMATED (len/4) — a deliberate, known asymmetry
		// with the cloud paths, which now meter on the provider's reported
		// usage object. Local backends are reached via the local router / node
		// path and surface only aggregate counts, not a per-request provider
		// usage object, so there is nothing exact to read here. It's harmless:
		// the billed COST is exactly 0 (local is free — the model isn't in the
		// price table), so the estimated token counts never affect spend, and
		// the row is marked not-estimated because the cost itself is exact (0).
		// Fold real usage in here only if/when local routing exposes a
		// per-request usage object.
		_ = p.alertManager.RecordSpend(ctx, wsID, team, sprint, feature, decision.Model, len(prompt)/4, len(formatted)/4, eventPrompt, sessionID, requestID, "text", false)
	}
	metrics.RequestsTotal.WithLabelValues(provider, "local").Inc()
	return true
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

// recordStreamSpend closes the streamed-spend gap. Streamed requests used to
// record no spend at all — invisible to budgets/alerts. This bills on the
// captured provider usage when present (cost_estimated=false), else a len/4
// estimate (cost_estimated=true), so a streamed request is never invisible.
// Respects the workspace logging policy (None opts out, like non-streaming).
// Prompt text isn't persisted on the streamed spend row (metadata-equivalent);
// the durable prompt record is the learner token-event written alongside.
func (p *Proxy) recordStreamSpend(ctx context.Context, sc streamSpend, u streamUsage, outputText string) {
	if p.alertManager == nil || sc.logging == workspace.LoggingNone {
		return
	}
	inT, outT := sc.estInputTokens, len(outputText)/4
	estimated := true
	source := "estimated"
	if u.present {
		inT, outT = u.inputTokens, u.outputTokens
		estimated = false
		source = "provider_usage"
	}
	metrics.SpendRecord(source)
	if err := p.alertManager.RecordSpend(ctx, sc.wsID, sc.team, sc.sprint, sc.feature, sc.model, inT, outT, "", sc.sessionID, sc.requestID, sc.modality, estimated); err != nil {
		slog.Warn("alerts: streamed RecordSpend failed", slog.String("err", err.Error()))
	}
	if p.budgetService != nil {
		p.budgetService.RecordSpend(ctx, sc.wsID, sc.team, sc.sprint, alerts.CostUSD(sc.model, inT, outT))
	}
	// Shadow LXC debit on the streaming path — same detached ctx as the
	// streamed cost_usd write above; void, observational.
	p.shadowSpendLXC(ctx, sc.wsID, alerts.CostUSD(sc.model, inT, outT))
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

func (p *Proxy) trySemantic(ctx context.Context, provider, model, prompt, workspaceID string) []byte {
	if p.semantic == nil {
		return nil
	}
	cached, err := p.semantic.Get(ctx, provider, model, prompt, workspaceID)
	if err != nil || cached == nil {
		return nil
	}
	return cached
}

// trySemanticPooled is the cross-tenant SEMANTIC read surface: a similarity
// search over is_poolable rows only (a separate keyspace from the private
// search, which filters those out). It embeds the RAW prompt — matching how
// pooled rows are written — and returns the body plus the contributing
// workspace plus the matched row's id and similarity (Stage-2.1 royalty
// attribution data). A miss is (nil, "", "", 0).
func (p *Proxy) trySemanticPooled(ctx context.Context, provider, model, rawPrompt string) ([]byte, string, string, float64) {
	if p.semantic == nil {
		return nil, "", "", 0
	}
	body, owner, entryID, sim, err := p.semantic.GetPooled(ctx, provider, model, rawPrompt)
	if err != nil || body == nil {
		return nil, "", "", 0
	}
	return body, owner, entryID, sim
}

// mintPooledRoyalty fires the Stage-2.1 Pool-B royalty mint for a SERVED
// pooled hit. Called ONLY at the serve points (after replayAsSSE succeeds or
// the cached body was written) — never at lookup. hit is nil for private hits
// and for non-pooled traffic, making this a no-op on every pre-Stage-2.1
// path; a nil royaltyMinter (flag off / not wired) is equally inert.
//
// avoided_COGS is what the requester's live call would have cost, priced with
// the existing estimated-tokens convention (len/4 — the proxy.go:1091 shape).
// Exactly-once enforcement lives in the Minter's request_id claim row; mint
// failure is logged and never affects the already-served response (the claim
// rolls back with the credit, so a later retry can still mint).
func (p *Proxy) mintPooledRoyalty(ctx context.Context, hit *poolroyalty.ServedHit, prompt string, served []byte, loggingPolicy workspace.LoggingPolicy) {
	if p.royaltyMinter == nil || hit == nil {
		return
	}
	// Stage 2.3.0 — NO HASH -> NO MINT (the privacy-coherence gate): a
	// none-LoggingPolicy requester forbids persisting content-derived
	// artifacts, so we capture no evidence hashes and fire no mint at all.
	// The serve itself already happened and is unaffected. Defense in depth:
	// the Minter independently refuses empty-hash hits.
	if loggingPolicy == workspace.LoggingNone {
		slog.Info("poolroyalty: mint skipped — requester logging policy 'none' forbids evidence capture (no hash, no mint)",
			slog.String("request_id", hit.RequestID),
			slog.String("contributor", hit.ContributorWorkspace),
		)
		return
	}
	// Evidence hashes, computed AT SERVE over the served cache-entry bytes
	// (on the SSE branch the wire format is synthesized FROM these bytes —
	// the entry, not the frames, is the adjudicable artifact): both cache
	// stores are mutable underneath the mint (Redis SET overwrite/TTL;
	// semantic upsert replaces response), so only a serve-time hash binds
	// the adjudicable content. Unsalted pure content hashes — the salted
	// identities already live on the claim row via entry_id. NOTE the
	// privacy posture: metadata-policy requesters persist NO prompt text,
	// but DO persist this prompt digest (pseudonymous, same class as the
	// existing prompt_embeddings.prompt_hash) — only policy 'none' opts a
	// workspace out of digests, at the cost of its pooled serves not minting.
	hit.AnswerSHA256 = poolroyalty.SHA256Hex(served)
	hit.PromptSHA256 = poolroyalty.SHA256Hex([]byte(prompt))
	hit.AvoidedCOGSUSD = alerts.CostUSD(hit.Model, len(prompt)/4, len(served)/4)
	// The serve already happened — a client disconnect after receiving the
	// response must not cancel the contributor's royalty mid-transaction.
	// WithoutCancel keeps the request's values (trace) but detaches its
	// cancellation; the mint is synchronous-after-flush, so it adds no
	// client-visible latency either way.
	res, err := p.royaltyMinter.MintServedHit(context.WithoutCancel(ctx), *hit)
	if err != nil {
		// On a clean error the tx rolled back (claim + credit together) and a
		// retry can mint; on an ambiguous commit error the claim may have
		// persisted, in which case the retry is suppressed — deflationary
		// either way, never a double-mint.
		slog.Warn("poolroyalty: mint failed (response already served; claim rolled back or commit ambiguous — a retry may mint, and can never double-mint)",
			slog.String("request_id", hit.RequestID),
			slog.String("contributor", hit.ContributorWorkspace),
			slog.String("requester", hit.RequesterWorkspace),
			slog.String("error", err.Error()),
		)
		return
	}
	if res.Capped {
		// 2.3b: serve-but-skip, same shape as the no-hash gate — the customer
		// was served before the mint ran; the pair's window budget is spent.
		slog.Info("poolroyalty: mint skipped — window cap reached (serve unaffected; exposure bounded)",
			slog.String("cap", res.CapReason),
			slog.String("request_id", hit.RequestID),
			slog.String("contributor", hit.ContributorWorkspace),
			slog.String("requester", hit.RequesterWorkspace),
		)
		return
	}
	if res.Minted {
		slog.Info("poolroyalty: royalty minted",
			slog.String("request_id", hit.RequestID),
			slog.String("contributor", hit.ContributorWorkspace),
			slog.String("requester", hit.RequesterWorkspace),
			slog.String("layer", hit.Layer),
			slog.Float64("amount", res.Amount),
		)
	}
}

// poolKeyMarker namespaces the shared (cross-tenant) cache keyspace so it is
// PROVABLY disjoint from the workspace-private keyspace. A private key is hashed
// from "wsID:prompt" where wsID comes from the X-Talyvor-Workspace HTTP header
// (which cannot contain NUL); the marker's NUL bytes therefore can never appear
// at the start of a private key's pre-image. Without this, a tenant could craft
// a raw prompt equal to "victimWsID:victimPrompt" and collide with the victim's
// PRIVATE key — reading a private entry that has no intentionally-pooled twin
// (e.g. one written before the victim opted in). The marker makes that
// impossible by construction, independent of the downstream consent check.
const poolKeyMarker = "\x00pool\x00"

// pooledPromptKey is the key material for the shared pool: the raw prompt under
// the reserved marker. Two poolable workspaces sending the same prompt land on
// the same pooled key (intended sharing); it can never equal a private key.
func pooledPromptKey(prompt string) string { return poolKeyMarker + prompt }

// tryExactPooled looks up the shared POOLED key (marker + raw prompt, no wsID)
// and returns the cached body plus the contributing workspace recorded on it.
// It is the cross-tenant read surface: a separate keyspace from the
// workspace-private keys tryExact uses, so it can never leak a private entry. A
// miss is (nil, "").
func (p *Proxy) tryExactPooled(ctx context.Context, provider, model, rawPrompt string) ([]byte, string) {
	if p.exact == nil {
		return nil, ""
	}
	body, owner, err := p.exact.GetWithOwner(ctx, provider, model, pooledPromptKey(rawPrompt))
	if err != nil || body == nil {
		return nil, ""
	}
	return body, owner
}

// storeCaches writes the response to the workspace-private exact key (now
// owner-tagged via SetWithOwner — additive, the private key is unchanged) and to
// the semantic cache. When the contributing workspace has opted into pooling AND
// the global switch is on, it ALSO writes a copy under the un-prefixed pooled
// key, tagged with the contributor — the only cross-tenant-readable surface.
// Inert by default: a nil/off gate writes no pooled copy. Callers gate this on
// !piiDetected, so a PII-flagged entry is never stored, hence never pooled.
func (p *Proxy) storeCaches(ctx context.Context, provider, model, cachePrompt, rawPrompt, wsID string, response []byte) {
	if p.exact != nil {
		// Private (workspace-scoped) entry — today's behavior, now owner-stamped.
		_ = p.exact.SetWithOwner(ctx, provider, model, cachePrompt, wsID, response)
		// Pooled (cross-tenant) copy under the reserved, namespace-disjoint pooled
		// key — opt-in, inert by default.
		if p.poolGate.DecidePoolableOnWrite(ctx, wsID) {
			_ = p.exact.SetWithOwner(ctx, provider, model, pooledPromptKey(rawPrompt), wsID, response)
		}
	}
	if p.semantic != nil && p.embedder != nil {
		// Private (workspace-scoped) semantic entry — embeds the wsID-prefixed
		// prompt and stores is_poolable=false (default), exactly as before.
		if vec, err := p.embedder.Embed(ctx, cachePrompt); err == nil {
			_ = p.semantic.Set(ctx, provider, model, cachePrompt, response, vec, wsID)
		}
		// Pooled (cross-tenant) semantic copy — opt-in, inert by default. Keyed on
		// the NUL-sentinel pooled prompt (disjoint hash) but embedding the RAW
		// prompt so cross-tenant similar prompts match cleanly; tagged with the
		// contributor + is_poolable=true.
		if p.poolGate.DecidePoolableOnWrite(ctx, wsID) {
			if vec, err := p.embedder.Embed(ctx, rawPrompt); err == nil {
				_ = p.semantic.SetPooled(ctx, provider, model, pooledPromptKey(rawPrompt), wsID, response, vec)
			}
		}
	}
}

// forward wraps the upstream call in retry.Do so transient 429/5xx
// responses are retried with exponential backoff before the proxy gives
// up. The closure builds a fresh request each attempt (bytes.NewReader
// keeps the body re-readable). Returns the final response, its body,
// the attempt count, and any non-retryable error.
func (p *Proxy) forward(ctx context.Context, r *http.Request, body []byte, model string, cfg providerConfig) (resp *http.Response, respBody []byte, attempts int, err error) {
	// Observe upstream provider latency + outcome on EVERY return path. The
	// named returns let one deferred RecordUpstream cover both success and
	// error without restructuring the call below. Bounded labels only; this
	// never alters control flow or the returned error.
	start := time.Now()
	defer func() {
		metrics.RecordUpstream(upstreamProviderLabel(cfg.name), upstreamStatusClass(resp, err), time.Since(start))
	}()

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
	resp = result.Response
	defer resp.Body.Close()

	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, result.Attempts, fmt.Errorf("read upstream response: %w", err)
	}
	return resp, respBody, result.Attempts, nil
}

// upstreamProviderLabel guards the provider metric label so an empty provider
// can never produce a blank series (and never panics).
func upstreamProviderLabel(provider string) string {
	if provider == "" {
		return "unknown"
	}
	return provider
}

// upstreamStatusClass normalizes an upstream result to a BOUNDED status label:
// "1xx"/"2xx"/"3xx"/"4xx"/"5xx", or "error" for a transport failure / no
// response. Never the raw code or error string (cardinality).
func upstreamStatusClass(resp *http.Response, err error) string {
	if err != nil || resp == nil {
		return "error"
	}
	switch resp.StatusCode / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	default:
		return "error"
	}
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

// disableStream sets the top-level "stream" field to false on a canonical
// (OpenAI-shape) request body so the upstream returns a non-streaming
// completion. Used when buffering for output guardrails. Every OpenAI-
// compatible provider (incl. Anthropic, which Lens forwards verbatim) reads
// this field; Google uses a non-streaming endpoint regardless. Best-effort:
// on a parse error the body is returned unchanged.
func disableStream(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["stream"] = false
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
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
	case "mistral":
		base := p.mistralURL
		key := p.mistralKey
		return providerConfig{
			name:          "mistral",
			upstreamURLFn: func(string) string { return base + "/v1/chat/completions" },
			setAuth: func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+key)
			},
		}
	case "groq":
		base := p.groqURL
		key := p.groqKey
		return providerConfig{
			name:          "groq",
			upstreamURLFn: func(string) string { return base + "/openai/v1/chat/completions" },
			setAuth: func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+key)
			},
		}
	case "vllm":
		base := p.vllmURL
		key := p.vllmKey
		return providerConfig{
			name:          "vllm",
			upstreamURLFn: func(string) string { return base + "/v1/chat/completions" },
			setAuth: func(req *http.Request) {
				// vLLM commonly runs without auth on private networks;
				// only attach the header when an operator-supplied key
				// is configured.
				if key != "" {
					req.Header.Set("Authorization", "Bearer "+key)
				}
			},
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
