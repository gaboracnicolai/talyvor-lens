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
	"strconv"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/ab"
	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/learner"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/quality"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/templates"
)

const (
	maxBodyBytes        = 4 << 20 // 4 MiB
	openAIChatURL       = "https://api.openai.com/v1/chat/completions"
	anthropicMessageURL = "https://api.anthropic.com/v1/messages"
	upstreamTimeout     = 120 * time.Second
)

type Proxy struct {
	exact            *cache.ExactCache
	semantic         *cache.SemanticCache
	embedder         cache.Embedder
	compressor       *compressor.Compressor
	router           *router.Router
	piiDetector      *pii.Detector
	alertManager     *alerts.AlertManager
	templateDetector *templates.TemplateDetector
	scorer           *quality.Scorer
	abTester         *ab.Tester
	tracker          *attribution.Tracker
	httpClient       *http.Client
	openAIKey        string
	anthropicKey     string
	learner          *learner.Learner

	// Upstream URLs are unexported and defaulted so tests can swap them
	// for an httptest server without leaking config to callers.
	openAIURL    string
	anthropicURL string
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
	openAIKey string,
	anthropicKey string,
	learners ...*learner.Learner,
) *Proxy {
	p := &Proxy{
		exact:            exactCache,
		semantic:         semanticCache,
		embedder:         embedder,
		compressor:       compressorImpl,
		router:           routerImpl,
		piiDetector:      piiDetector,
		alertManager:     alertManager,
		templateDetector: templateDetector,
		scorer:           scorer,
		abTester:         abTester,
		tracker:          tracker,
		httpClient:       &http.Client{Timeout: upstreamTimeout},
		openAIKey:        openAIKey,
		anthropicKey:     anthropicKey,
		openAIURL:        openAIChatURL,
		anthropicURL:     anthropicMessageURL,
	}
	if len(learners) > 0 {
		p.learner = learners[0]
	}
	return p
}

// providerConfig holds the per-provider knobs HandleOpenAI/HandleAnthropic
// differ on. Everything else is shared in serve().
type providerConfig struct {
	name        string
	upstreamURL string
	setAuth     func(*http.Request)
}

func (p *Proxy) HandleOpenAI(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, providerConfig{
		name:        "openai",
		upstreamURL: p.openAIURL,
		setAuth: func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+p.openAIKey)
		},
	})
}

func (p *Proxy) HandleAnthropic(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, providerConfig{
		name:        "anthropic",
		upstreamURL: p.anthropicURL,
		setAuth: func(req *http.Request) {
			req.Header.Set("x-api-key", p.anthropicKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		},
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

	if !piiDetected {
		if cached := p.tryExact(ctx, cfg.name, model, prompt); cached != nil {
			writeBytes(w, http.StatusOK, cached)
			metrics.RequestsTotal.WithLabelValues(cfg.name, "cache_hit_exact").Inc()
			return
		}
		if cached := p.trySemantic(ctx, cfg.name, model, prompt); cached != nil {
			writeBytes(w, http.StatusOK, cached)
			metrics.RequestsTotal.WithLabelValues(cfg.name, "cache_hit_semantic").Inc()
			return
		}
	}

	// Streaming path: detected by "stream": true in the request JSON. The
	// stream handler forwards SSE chunks unbuffered, then caches the
	// assembled response after the upstream stream completes. We skip the
	// compression + routing path for streams since that would rewrite the
	// body and break wire-compatibility with the live SSE.
	if streamRequested(body) {
		sh := &StreamHandler{proxy: p}
		var serr error
		if cfg.name == "openai" {
			serr = sh.ServeOpenAI(w, r, cfg.name, model, prompt, body, piiDetected)
		} else {
			serr = sh.ServeAnthropic(w, r, cfg.name, model, prompt, body, piiDetected)
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

	upstreamResp, upstreamBody, err := p.forward(ctx, r, upstreamBodyOut, cfg)
	if err != nil {
		metrics.RequestsTotal.WithLabelValues(cfg.name, "error").Inc()
		writeError(w, http.StatusBadGateway, "upstream LLM error: "+err.Error())
		return
	}

	// Score the response so we can gate caching on quality. Scoring is
	// pure-Go heuristics — fast enough to do on the hot path. Score is
	// only meaningful for a successful upstream (200); on errors we skip
	// scoring entirely.
	var qualityScore *quality.QualityScore
	if p.scorer != nil && upstreamResp.StatusCode == http.StatusOK {
		q := p.scorer.ScoreResponse(ctx, prompt, string(upstreamBody), cfg.name, model)
		qualityScore = &q
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
	if ct := upstreamResp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(upstreamResp.StatusCode)
	_, _ = w.Write(upstreamBody)

	if upstreamResp.StatusCode == http.StatusOK {
		// Cache iff the prompt has no PII AND the response is judged
		// cacheable by the quality scorer. Low-quality responses are
		// forwarded to the client but never persisted.
		shouldCache := !piiDetected
		if qualityScore != nil && !qualityScore.ShouldCache {
			shouldCache = false
		}
		if shouldCache {
			// Cache against the original (uncompressed) prompt + originally
			// requested model so repeat callers get cache hits.
			p.storeCaches(ctx, cfg.name, model, prompt, upstreamBody)
		}
		eventPrompt := prompt
		if piiDetected {
			eventPrompt = redactedPrompt
		}
		p.recordTokenEvent(ctx, cfg.name, model, eventPrompt, upstreamBody, savingsPct, piiDetected)
		// RecordSpend prices the model that was actually billed by the
		// LLM (the upstream model, after any router or circuit override).
		// Fire-and-forget — alert manager failures must never break a
		// successful request.
		inT, outT := len(prompt)/4, len(upstreamBody)/4
		if p.alertManager != nil {
			if err := p.alertManager.RecordSpend(ctx, team, feature, upstreamModel, inT, outT); err != nil {
				slog.Warn("alerts: RecordSpend failed",
					slog.String("err", err.Error()),
				)
			}
		}
		// Branch / PR attribution is also best-effort: DB errors here must
		// not propagate to the caller. The cost is computed against the
		// upstream model so the same number lands in alerts and attribution.
		if willAttribute {
			cost := alerts.CostUSD(upstreamModel, inT, outT)
			if err := p.tracker.Record(ctx, attr, upstreamModel, inT, outT, cost); err != nil {
				slog.Warn("attribution: Record failed",
					slog.String("err", err.Error()),
				)
			}
		}
		p.launchABShadows(cfg.name, model, prompt, body)
		metrics.RequestsTotal.WithLabelValues(cfg.name, "forwarded").Inc()
	} else {
		metrics.RequestsTotal.WithLabelValues(cfg.name, "upstream_error").Inc()
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

func (p *Proxy) forward(ctx context.Context, r *http.Request, body []byte, cfg providerConfig) (*http.Response, []byte, error) {
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build upstream request: %w", err)
	}

	for name, values := range r.Header {
		if strings.EqualFold(name, "Host") {
			continue
		}
		for _, v := range values {
			upstreamReq.Header.Add(name, v)
		}
	}
	cfg.setAuth(upstreamReq)

	resp, err := p.httpClient.Do(upstreamReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read upstream response: %w", err)
	}
	return resp, respBody, nil
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
