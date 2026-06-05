package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/workspace"
)

// distillIntegration wires the request path to the DISTILL orchestrator: it
// converts a document carried in a chat request to clean Markdown via the
// ISOLATED subprocess BEFORE the model call, when the workspace + request opt
// in. It is inert-by-default — MaybeDistill rewrites nothing unless a document
// is present AND policy + opt-in both allow it AND the conversion succeeds. A
// conversion failure never fails the user's request.
type distillIntegration struct {
	converter distill.IsolatedConverter // *distill.ProcessIsolator (killable subprocess)
	cache     distill.Cache             // optional conversion cache (may be nil)
	wsManager *workspace.Manager
}

// SetDistiller enables the request-path DISTILL integration. converter is the
// isolated subprocess (*distill.ProcessIsolator); cache is the optional
// conversion cache (nil disables it). Wired as a setter so proxy.New's
// signature stays put — when unset, distillation is fully inert.
func (p *Proxy) SetDistiller(converter distill.IsolatedConverter, cache distill.Cache) {
	p.distiller = &distillIntegration{
		converter: converter,
		cache:     cache,
		wsManager: p.workspaceManager,
	}
}

// MaybeDistill returns a possibly-rewritten request body plus the re-derived
// prompt + modality. didDistill reports whether anything changed; when false,
// the returned body is the SAME slice and the caller's flow is untouched.
// Untrusted document bytes are converted ONLY through the isolated converter;
// the JSON envelope is parsed in-process (safe — standard encoding/json, the
// same parsing serve() already does via extractPrompt).
// vision is the OPTIONAL live vision-OCR dispatcher (nil = no live vision): when
// a document is text-less (NeedsVision), it is OCR'd via a vision-capable model
// and the COST is booked honestly (see the orchestrator's visionFallback). A nil
// dispatcher keeps the prior behavior — a NeedsVision document passes through.
func (d *distillIntegration) MaybeDistill(ctx context.Context, r *http.Request, body []byte, wsID string, modSet modality.ModalitySet, vision distill.VisionDispatcher) ([]byte, string, modality.ModalitySet, bool) {
	// Fail-safe: a misconfigured integration is inert, never a panic on the
	// shared request path (production always wires both, but the inert/graceful
	// contract must hold regardless).
	if d == nil || d.converter == nil || d.wsManager == nil {
		return body, "", modSet, false
	}
	// Gate 1: a document-or-image block must be present (cheap; already detected
	// upstream). We include HasImage because some clients send a PDF as an
	// OpenAI image_url data-URI (media_type application/pdf) — modality flags it
	// as an image since it keys on the block type, not the declared media type.
	// A genuine image (image/png) passes this gate but is filtered out per-block
	// by FormatFromMediaType, so it is never converted.
	if !modSet.HasDocument && !modSet.HasImage {
		return body, "", modSet, false
	}
	// Gate 2: workspace policy + per-request opt-in.
	if !d.shouldDistill(r, wsID) {
		return body, "", modSet, false
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body, "", modSet, false // malformed envelope → leave the normal path to handle it
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return body, "", modSet, false
	}

	distilledAny := false
	for _, mi := range msgs {
		msg, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue // string content carries no document block
		}
		newBlocks := make([]any, 0, len(blocks))
		msgChanged := false
		for _, bi := range blocks {
			if block, ok := bi.(map[string]any); ok {
				if md, ok := d.tryConvertBlock(ctx, block, vision); ok {
					newBlocks = append(newBlocks, map[string]any{"type": "text", "text": md})
					msgChanged = true
					distilledAny = true
					continue
				}
			}
			newBlocks = append(newBlocks, bi)
		}
		if msgChanged {
			// Collapse to a single clean text string when every block is now
			// text (the common case: "here's a PDF, summarize" → clean
			// Markdown). Keep the array when a non-text block (e.g. an image)
			// remains — that request is genuinely still multimodal.
			if joined, allText := joinIfAllText(newBlocks); allText {
				msg["content"] = joined
			} else {
				msg["content"] = newBlocks
			}
		}
	}

	if !distilledAny {
		// Nothing converted (NeedsVision / unsupported / error) → inert.
		return body, "", modSet, false
	}

	newBody, err := json.Marshal(root)
	if err != nil {
		return body, "", modSet, false // marshal failure → fail safe to the original
	}
	// Re-derive everything that was computed from the original body.
	_, newPrompt, perr := extractPrompt(newBody)
	if perr != nil {
		return body, "", modSet, false
	}
	return newBody, newPrompt, modality.Detect(newBody), true
}

// shouldDistill applies the policy + per-request opt-in rules: a document is
// distilled only if the workspace allows it AND (policy is always-on OR the
// request carries X-Talyvor-Distill: true).
func (d *distillIntegration) shouldDistill(r *http.Request, wsID string) bool {
	switch d.wsManager.GetDistillPolicy(wsID) {
	case workspace.DistillAlways:
		return true
	case workspace.DistillOptIn:
		return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Talyvor-Distill")), "true")
	default: // DistillDisabled
		return false
	}
}

// tryConvertBlock converts one content block if it is a distillable document.
// Returns ok=false (block left untouched) for non-documents, images,
// unsupported types, an UNRESOLVED NeedsVision result, and any conversion error
// — the graceful path that never fails a request. When vision is non-nil and the
// document is text-less, the orchestrator OCRs it (NeedsVision resolves to the
// recovered Markdown, its token cost booked honestly inside Orchestrate); when
// vision is nil or the OCR fails, the result stays NeedsVision and the block is
// left untouched.
func (d *distillIntegration) tryConvertBlock(ctx context.Context, block map[string]any, vision distill.VisionDispatcher) (string, bool) {
	raw, mediaType, ok := extractBlockDocument(block)
	if !ok {
		return "", false
	}
	format, ok := distill.FormatFromMediaType(mediaType)
	if !ok {
		return "", false // e.g. image/png — a vision input, not a document
	}
	res, _, err := distill.Orchestrate(ctx, d.converter, d.cache, vision, raw, format, distill.TierFaithful)
	if err != nil || res.NeedsVision || strings.TrimSpace(res.Markdown) == "" {
		return "", false
	}
	return res.Markdown, true
}

// extractBlockDocument pulls the raw bytes + media type from a content block.
// Handles the Anthropic source-block shape (type document/image, base64 source)
// and the OpenAI data-URI shape (image_url / file with a data: URL).
func extractBlockDocument(block map[string]any) (data []byte, mediaType string, ok bool) {
	switch block["type"] {
	case "document", "image":
		src, _ := block["source"].(map[string]any)
		if src == nil {
			return nil, "", false
		}
		if st, _ := src["type"].(string); st != "base64" {
			return nil, "", false
		}
		mt, _ := src["media_type"].(string)
		raw, err := base64.StdEncoding.DecodeString(stringOf(src["data"]))
		if err != nil || mt == "" || len(raw) == 0 {
			return nil, "", false
		}
		return raw, mt, true
	case "image_url":
		iu, _ := block["image_url"].(map[string]any)
		if iu == nil {
			return nil, "", false
		}
		return parseDataURI(stringOf(iu["url"]))
	case "file":
		if f, _ := block["file"].(map[string]any); f != nil {
			if raw, mt, ok := parseDataURI(stringOf(f["file_data"])); ok {
				return raw, mt, true
			}
		}
		return nil, "", false
	}
	return nil, "", false
}

// parseDataURI decodes a "data:<media-type>;base64,<data>" URI.
func parseDataURI(uri string) (data []byte, mediaType string, ok bool) {
	if !strings.HasPrefix(uri, "data:") {
		return nil, "", false
	}
	rest := uri[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return nil, "", false
	}
	meta, payload := rest[:comma], rest[comma+1:]
	if !strings.Contains(meta, ";base64") {
		return nil, "", false
	}
	mt := meta
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || mt == "" || len(raw) == 0 {
		return nil, "", false
	}
	return raw, mt, true
}

// joinIfAllText collapses a content-block slice to a single newline-joined text
// string IF every block is a text block; otherwise allText=false and the caller
// keeps the array (a genuine multimodal request).
func joinIfAllText(blocks []any) (string, bool) {
	parts := make([]string, 0, len(blocks))
	for _, bi := range blocks {
		b, ok := bi.(map[string]any)
		if !ok || b["type"] != "text" {
			return "", false
		}
		parts = append(parts, stringOf(b["text"]))
	}
	return strings.Join(parts, "\n\n"), true
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}
