package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/inference"
	"github.com/talyvor/lens/internal/modality"
)

// vision_dispatch.go is the LIVE side of the DISTILL stage-5 vision seam: it
// turns a NeedsVision (scanned / text-less) document into real OCR by sending it
// to a vision/document-capable model and reporting the call's REAL token cost.
//
// HONESTY (the cardinal rule): vision-OCR SPENDS tokens — it never saves them.
// This file only RECOVERS the text and reports the cost; the orchestrator's
// visionFallback books that cost as a Savings.VisionTokensCost (with
// TokensSaved == 0) so a net-negative DISTILL outcome surfaces honestly.
//
// GRACEFUL FAILURE: every failure mode (no capable model, an unsupported
// provider request path, a transport/upstream error, an empty response) returns
// an error. The orchestrator's visionFallback turns that error into a no-op —
// the result stays NeedsVision and the request path passes the ORIGINAL document
// through unchanged. A vision outage never fails a user's request.

// visionMaxTokens caps the OCR transcription length. Generous enough for a
// multi-page document while bounding a runaway response.
const visionMaxTokens = 4096

// visionDispatcher is the proxy-side distill.VisionDispatcher. forward is
// injected so the dispatch logic (model selection, request shaping, cost
// extraction) is unit-testable without a live provider; production wires it to
// p.forward against the target provider.
type visionDispatcher struct {
	provider      string   // the INCOMING request's provider — we never silently hop providers
	allowedModels []string // the workspace allow-list (empty = any model)
	maxTokens     int      // OCR output cap
	forward       func(ctx context.Context, body []byte, model string) (respBody []byte, status int, err error)
}

// newVisionDispatcher builds the live dispatcher for one request. It targets the
// SAME provider the request is already using (no cross-provider hop — that would
// bring surprising keys/cost and bypass the workspace's provider allow-list),
// scoped to the workspace's allowed models. A nil return is impossible; an
// unusable configuration simply fails gracefully at dispatch time.
func (p *Proxy) newVisionDispatcher(r *http.Request, cfg providerConfig, wsID string) distill.VisionDispatcher {
	var allowed []string
	if p.workspaceManager != nil {
		if ws, ok := p.workspaceManager.GetWorkspace(wsID); ok && ws != nil {
			allowed = ws.AllowedModels
		}
	}
	provider := cfg.ProviderName()
	return &visionDispatcher{
		provider:      provider,
		allowedModels: allowed,
		maxTokens:     visionMaxTokens,
		// Use forward() (NOT forwardWithFallback): the fallback router could
		// route the document to a non-document model (e.g. OpenAI), which would
		// drop the document and hallucinate fake "OCR". A single, capability-
		// gated provider call is the honest choice.
		forward: func(ctx context.Context, body []byte, model string) ([]byte, int, error) {
			resp, rb, _, err := p.forward(ctx, r, body, model, cfg)
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			return rb, status, err
		},
	}
}

// PlannedVisionModel reports the model DispatchVision WOULD OCR with — without
// dispatching — by running the SAME two gates (provider preserves documents +
// a document-capable model exists in the allow-list). It satisfies
// distill.ModelPlanner so the orchestrator can key the OCR-result cache on the
// model before the call. ok=false (unsupported provider / no capable model) → the
// orchestrator skips the OCR cache, and the eventual dispatch fails gracefully the
// same way. MUST stay in lockstep with DispatchVision's gates 1+2 — keying on a
// model the dispatch then wouldn't use would file the entry under the wrong key.
func (d *visionDispatcher) PlannedVisionModel(_ context.Context) (string, bool) {
	if !visionProviderSupported(d.provider) {
		return "", false
	}
	return modality.CapableModel(d.provider, modality.ModalitySet{HasDocument: true}, d.allowedModels)
}

// DispatchVision selects a document-capable model within the workspace allow-
// list, OCRs the document, and reports the real token cost. Any failure returns
// an error (→ graceful passthrough). It NEVER returns partial/empty text as a
// success: an empty transcription is a failure, not a zero-cost saving.
func (d *visionDispatcher) DispatchVision(ctx context.Context, req distill.VisionRequest) (distill.VisionResult, error) {
	// Gate 1: the provider's request path must PRESERVE the document. Anthropic
	// (native passthrough) and Bedrock (Anthropic-shaped translate) carry a
	// document block intact; Gemini's translation flattens content to plain text
	// and would drop the document, so we refuse rather than fake an OCR.
	if !visionProviderSupported(d.provider) {
		return distill.VisionResult{}, fmt.Errorf("vision-OCR unavailable for provider %q: its request path does not preserve documents", d.provider)
	}

	// Gate 2: a document-capable model must exist within the allow-list.
	model, ok := modality.CapableModel(d.provider, modality.ModalitySet{HasDocument: true}, d.allowedModels)
	if !ok {
		return distill.VisionResult{}, fmt.Errorf("no document-capable model allowed for provider %q", d.provider)
	}

	body, err := buildAnthropicVisionBody(model, d.maxTokens, req)
	if err != nil {
		return distill.VisionResult{}, fmt.Errorf("build vision request: %w", err)
	}

	rb, status, err := d.forward(ctx, body, model)
	if err != nil {
		return distill.VisionResult{}, fmt.Errorf("vision forward: %w", err)
	}
	if status != http.StatusOK {
		return distill.VisionResult{}, fmt.Errorf("vision call returned HTTP %d", status)
	}

	text := visionResponseText(d.provider, rb)
	if strings.TrimSpace(text) == "" {
		return distill.VisionResult{}, fmt.Errorf("vision response carried no transcribed text")
	}

	in, out := visionCost(d.provider, rb, req.Bytes, text)
	return distill.VisionResult{
		Markdown:     text,
		InputTokens:  in,
		OutputTokens: out,
		Model:        model,
	}, nil
}

// visionProviderSupported reports whether the provider's request path preserves
// a document content block. Kept deliberately narrow: only providers proven to
// carry the document survive here. Bedrock (Anthropic-shaped translate) is a
// near-trivial future addition once its response extraction is exercised; Gemini
// is intentionally excluded (its translation drops documents).
func visionProviderSupported(provider string) bool {
	return provider == "anthropic"
}

// buildAnthropicVisionBody shapes an Anthropic-native messages request: a single
// user turn with a base64 document block followed by the OCR prompt. Anthropic
// accepts a base64 PDF directly, so no page rendering is needed.
func buildAnthropicVisionBody(model string, maxTokens int, req distill.VisionRequest) ([]byte, error) {
	mediaType := req.MediaType
	if mediaType == "" {
		mediaType = "application/pdf"
	}
	envelope := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "document",
						"source": map[string]any{
							"type":       "base64",
							"media_type": mediaType,
							"data":       base64.StdEncoding.EncodeToString(req.Bytes),
						},
					},
					map[string]any{"type": "text", "text": req.Prompt},
				},
			},
		},
	}
	return json.Marshal(envelope)
}

// visionResponseText pulls the transcribed text out of the upstream response,
// provider-aware: Anthropic responses stay native (content blocks); every other
// supported provider is reverse-translated to OpenAI shape by the time it lands.
func visionResponseText(provider string, body []byte) string {
	if provider == "anthropic" {
		return extractAnthropicText(body)
	}
	return extractResponseContent(body)
}

// extractAnthropicText concatenates the text blocks of a native Anthropic
// messages response, ignoring non-text blocks (e.g. thinking). A malformed body
// yields "" (→ a dispatch error, → graceful passthrough).
func extractAnthropicText(body []byte) string {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &r) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// visionCost returns the OCR call's token cost: the provider's REPORTED usage
// when present, else a conservative non-zero estimate. It must never report a
// free OCR — under-reporting the cost would understate (or fake) the DISTILL
// savings, violating the honesty rule.
func visionCost(provider string, respBody, docBytes []byte, text string) (in, out int) {
	// Trust the provider's REPORTED usage only when it is present AND non-zero.
	// A present-but-all-zero usage block alongside recovered text is as
	// untrustworthy as a missing one — accepting it would book a FREE OCR, so we
	// fall through to the non-zero estimate instead.
	if u, ok := inference.ExtractUsage(provider, respBody); ok && (u.InputTokens > 0 || u.OutputTokens > 0) {
		return u.InputTokens, u.OutputTokens
	}
	// No usable reported usage: estimate. Document input is dominated by the rendered
	// pages, which we can't measure here, so floor the input at the byte/4
	// estimate (a deliberate under-floor, never zero) and estimate the output
	// from the transcription length.
	in = len(docBytes) / 4
	if in <= 0 {
		in = 1
	}
	out = len(text) / 4
	if out <= 0 {
		out = 1
	}
	return in, out
}
