package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/workspace"
)

// distillPoolMarker namespaces the SHARED (cross-tenant) distill keyspace so it
// is provably disjoint from every workspace's private keyspace. A private key
// material is "wsID:contentHash" where wsID comes from the authenticated
// credential / X-Talyvor-Workspace header (which cannot contain NUL); the
// marker's leading NUL therefore can never begin a private key's pre-image.
const distillPoolMarker = "\x00distillpool\x00"

// ownerDistillCache is the pooled-keyspace capability the cross-tenant distill
// share needs: store/read an artifact stamped with its contributing workspace.
// *cache.DistillCache satisfies it; a test fake that implements only
// distill.Cache simply disables pooling (the type assertion fails — nil-safe).
type ownerDistillCache interface {
	GetWithOwner(ctx context.Context, contentHash, version string) ([]byte, string, error)
	SetWithOwner(ctx context.Context, contentHash, version, owner string, value []byte) error
}

// scopedDistillCache namespaces the distill cache key by prefixing the content
// hash with a per-workspace scope, making the cache PRIVATE (per-workspace) by
// default — this is what closes the previously-ungated cross-tenant flow. The
// "wsID:" prefix can never collide with the pooled keyspace's NUL marker.
type scopedDistillCache struct {
	inner distill.Cache
	scope string // e.g. "ws-123:" — prepended to the content hash
}

func (s scopedDistillCache) Get(ctx context.Context, contentHash, version string) ([]byte, error) {
	return s.inner.Get(ctx, s.scope+contentHash, version)
}

func (s scopedDistillCache) Set(ctx context.Context, contentHash, version string, value []byte) error {
	return s.inner.Set(ctx, s.scope+contentHash, version, value)
}

// distillIntegration wires the request path to the DISTILL orchestrator: it
// converts a document carried in a chat request to clean Markdown via the
// ISOLATED subprocess BEFORE the model call, when the workspace + request opt
// in. It is inert-by-default — MaybeDistill rewrites nothing unless a document
// is present AND policy + opt-in both allow it AND the conversion succeeds. A
// conversion failure never fails the user's request.
type distillIntegration struct {
	converter distill.IsolatedConverter      // *distill.ProcessIsolator (killable subprocess)
	cache     distill.Cache                  // optional conversion cache (may be nil)
	wsManager *workspace.Manager             //
	poolGate  *cache_pooling.PoolabilityGate // cross-tenant distill-share consent (nil-safe; nil = private-only)
}

// SetDistiller enables the request-path DISTILL integration. converter is the
// isolated subprocess (*distill.ProcessIsolator); cache is the optional
// conversion cache (nil disables it); poolGate gates cross-tenant distill
// sharing (nil = strictly private); attribSink is the S1 mint-free attribution
// sink (nil = attribution off). Wired as a setter so proxy.New's signature stays
// put — when unset, distillation is fully inert, with poolGate off/nil the cache
// is strictly per-workspace, and with attribSink nil nothing is attributed.
func (p *Proxy) SetDistiller(converter distill.IsolatedConverter, cache distill.Cache, poolGate *cache_pooling.PoolabilityGate, attribSink distillAttributionSink) {
	p.distiller = &distillIntegration{
		converter: converter,
		cache:     cache,
		wsManager: p.workspaceManager,
		poolGate:  poolGate,
	}
	p.distillAttribSink = attribSink
}

// MaybeDistill returns a possibly-rewritten request body plus the re-derived
// prompt + modality. didDistill reports whether anything changed; when false,
// the returned body is the SAME slice and the caller's flow is untouched.
// Untrusted document bytes are converted ONLY through the isolated converter;
// the JSON envelope is parsed in-process (safe — standard encoding/json, the
// same parsing serve() already does via extractPrompt).
// visionSpend is the OCR sub-call cost (tokens + the model that served it),
// surfaced from MaybeDistill so the request path can book a durable, model-priced
// 'vision_ocr' token_events row distinct from the 'convert' main row. The zero
// value means no OCR happened.
type visionSpend struct {
	model        string
	inputTokens  int
	outputTokens int
}

// recorded reports whether any OCR cost was incurred (and so a vision_ocr row is
// owed). A zero-token OCR is treated as none.
func (v visionSpend) recorded() bool { return v.inputTokens > 0 || v.outputTokens > 0 }

// vision is the OPTIONAL live vision-OCR dispatcher (nil = no live vision): when
// a document is text-less (NeedsVision), it is OCR'd via a vision-capable model
// and the COST is booked honestly (see the orchestrator's visionFallback). A nil
// dispatcher keeps the prior behavior — a NeedsVision document passes through.
//
// The returned visionSpend carries the OCR sub-call's token cost + model (summed
// across any OCR'd blocks) so the caller can record a durable 'vision_ocr' spend
// row; it is the zero value when no OCR happened.
func (d *distillIntegration) MaybeDistill(ctx context.Context, r *http.Request, body []byte, wsID string, modSet modality.ModalitySet, vision distill.VisionDispatcher) ([]byte, string, modality.ModalitySet, bool, visionSpend, []distillServeFact) {
	var ocr visionSpend
	// Fail-safe: a misconfigured integration is inert, never a panic on the
	// shared request path (production always wires both, but the inert/graceful
	// contract must hold regardless).
	if d == nil || d.converter == nil || d.wsManager == nil {
		return body, "", modSet, false, ocr, nil
	}
	// Gate 1: a document-or-image block must be present (cheap; already detected
	// upstream). We include HasImage because some clients send a PDF as an
	// OpenAI image_url data-URI (media_type application/pdf) — modality flags it
	// as an image since it keys on the block type, not the declared media type.
	// A genuine image (image/png) passes this gate but is filtered out per-block
	// by FormatFromMediaType, so it is never converted.
	if !modSet.HasDocument && !modSet.HasImage {
		return body, "", modSet, false, ocr, nil
	}
	// Gate 2: workspace policy + per-request opt-in.
	if !d.shouldDistill(r, wsID) {
		return body, "", modSet, false, ocr, nil
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body, "", modSet, false, ocr, nil // malformed envelope → leave the normal path to handle it
	}
	msgs, ok := root["messages"].([]any)
	if !ok {
		return body, "", modSet, false, ocr, nil
	}

	distilledAny := false
	var distillFacts []distillServeFact // S1: consented cross-tenant serves to attribute post-flush
	seenAttrib := map[string]bool{}     // dedup attribution per (owner, hash) within one request
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
				if md, vs, attrib, ok := d.tryConvertBlock(ctx, block, vision, wsID); ok {
					newBlocks = append(newBlocks, map[string]any{"type": "text", "text": md})
					msgChanged = true
					distilledAny = true
					// S1: collect the consented cross-tenant serve fact, deduped
					// per (owner, hash) so two identical blocks in one request count
					// as one serve event (the store would otherwise double-bump).
					if attrib != nil {
						if k := attrib.owner + "\x00" + attrib.hash; !seenAttrib[k] {
							seenAttrib[k] = true
							distillFacts = append(distillFacts, *attrib)
						}
					}
					// Accumulate OCR cost across blocks (same provider/model per
					// request, so summing in/out prices correctly on one row).
					if vs.recorded() {
						ocr.model = vs.model
						ocr.inputTokens += vs.inputTokens
						ocr.outputTokens += vs.outputTokens
					}
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
		return body, "", modSet, false, ocr, nil
	}

	newBody, err := json.Marshal(root)
	if err != nil {
		return body, "", modSet, false, ocr, nil // marshal failure → fail safe to the original
	}
	// Re-derive everything that was computed from the original body.
	_, newPrompt, perr := extractPrompt(newBody)
	if perr != nil {
		return body, "", modSet, false, ocr, nil
	}
	return newBody, newPrompt, modality.Detect(newBody), true, ocr, distillFacts
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
// left untouched. On a successful OCR the returned visionSpend carries the call's
// real token cost + model so the caller can book a durable 'vision_ocr' row; it
// is the zero value for a plain text conversion.
func (d *distillIntegration) tryConvertBlock(ctx context.Context, block map[string]any, vision distill.VisionDispatcher, wsID string) (string, visionSpend, *distillServeFact, bool) {
	raw, mediaType, ok := extractBlockDocument(block)
	if !ok {
		return "", visionSpend{}, nil, false
	}
	format, ok := distill.FormatFromMediaType(mediaType)
	if !ok {
		return "", visionSpend{}, nil, false // e.g. image/png — a vision input, not a document
	}

	hash := distill.ContentHash(raw)
	cacheVer := distill.CacheVersion(distill.TierFaithful)
	pooled, _ := d.cache.(ownerDistillCache) // nil when the cache can't carry an owner → pooling off

	// (1) POOLED READ — a cross-tenant artifact may be served ONLY when all
	//     three hold: the global switch is on, the REQUESTER opted in
	//     (Participant), AND the OWNER opted in (MaybeAllowPooledHit). With the
	//     switch off / no opt-in this is skipped and serving stays private.
	if pooled != nil && d.poolGate.Participant(wsID) {
		if b, owner, _ := pooled.GetWithOwner(ctx, distillPoolMarker+hash, cacheVer); len(b) > 0 &&
			d.poolGate.MaybeAllowPooledHit(ctx, wsID, owner) {
			if res, ok := distill.UnmarshalCached(b); ok && !res.NeedsVision && strings.TrimSpace(res.Markdown) != "" {
				// S1 attribution: emit the consented cross-tenant serve fact for
				// serve() to record post-flush — but SKIP self-serve, where a
				// poolable producer re-hits its OWN pooled artifact (owner == wsID;
				// MaybeAllowPooledHit is true there). The skip lives here, the one
				// place owner + requester are both in hand.
				var attrib *distillServeFact
				if owner != wsID {
					attrib = &distillServeFact{owner: owner, hash: hash}
				}
				return res.Markdown, visionSpend{}, attrib, true // cross-tenant serve (consented)
			}
		}
	}

	// (1b) POOLED OCR READ — a cross-tenant OCR TRANSCRIPTION (the recovered text
	//      of a scanned document — a MORE sensitive disclosure than a parsed
	//      document) may be served ONLY under the SAME full dual-consent as (1),
	//      over a DISJOINT keyspace (OCRCacheVersion's "ocr:" prefix). The PLANNED
	//      vision model keys the entry, mirroring orchestrateVision; a dispatcher
	//      that cannot plan a model (not a ModelPlanner, or ok=false) skips OCR
	//      pooling entirely (fail-safe — never a wrong-model serve). A hit serves
	//      the prior OCR WITHOUT dispatching vision (zero new spend). The S1
	//      attribution + the S4 royalty basis are PR2; PR1 serves only.
	if pooled != nil && vision != nil && d.poolGate.Participant(wsID) {
		if mp, okMP := vision.(distill.ModelPlanner); okMP {
			if planModel, okPlan := mp.PlannedVisionModel(ctx); okPlan && planModel != "" {
				if b, owner, _ := pooled.GetWithOwner(ctx, distillPoolMarker+hash, distill.OCRCacheVersion(planModel)); len(b) > 0 &&
					d.poolGate.MaybeAllowPooledHit(ctx, wsID, owner) {
					if co, okC := distill.UnmarshalCachedOCR(b); okC && !co.Result.NeedsVision && strings.TrimSpace(co.Result.Markdown) != "" {
						return co.Result.Markdown, visionSpend{}, nil, true // cross-tenant OCR serve (consented)
					}
				}
			}
		}
	}

	// (2) PRIVATE PATH (default) — a wsID-SCOPED cache, so an artifact is served
	//     only within its producing workspace. This is the leak fix.
	var privateCache distill.Cache
	if d.cache != nil {
		privateCache = scopedDistillCache{inner: d.cache, scope: wsID + ":"}
	}
	res, sav, err := distill.Orchestrate(ctx, d.converter, privateCache, vision, raw, format, distill.TierFaithful)
	if err != nil || res.NeedsVision || strings.TrimSpace(res.Markdown) == "" {
		return "", visionSpend{}, nil, false
	}

	// (3) POOLED WRITE — if the PRODUCER opted in (+ global switch), publish a
	//     NATIVE conversion to the shared keyspace stamped with this workspace as
	//     owner, so other opted-in tenants may be served it. Best-effort; never
	//     fails the request. NeedsVision/empty already rejected above. OCR results
	//     are EXCLUDED here — they are pooled by (3b) into the OCR keyspace WITH
	//     their avoided-COGS basis, so a cost-basis-less copy can't shadow them in
	//     the conversion keyspace (which (1) would otherwise serve first).
	if pooled != nil && res.Method != distill.MethodVisionOCR && d.poolGate.DecidePoolableOnWrite(ctx, wsID) {
		if b, mErr := distill.MarshalCached(res); mErr == nil {
			_ = pooled.SetWithOwner(ctx, distillPoolMarker+hash, cacheVer, wsID, b)
		}
	}

	// An OCR'd result carries the vision call's real token split + model so the
	// request path can book it as a distinct, model-priced 'vision_ocr' row.
	var vs visionSpend
	if res.Method == distill.MethodVisionOCR {
		vs = visionSpend{model: sav.VisionModel, inputTokens: sav.VisionInputTokens, outputTokens: sav.VisionOutputTokens}

		// (3b) POOLED OCR WRITE — if THIS workspace OCR'd the document AND opted
		//      in, publish the transcription to the shared OCR keyspace stamped as
		//      owner, keyed on the ACTUAL serving model (sav.VisionModel), so other
		//      opted-in tenants may be served it without re-dispatching vision.
		//      MarshalCachedOCR preserves the avoided-COGS basis (token split +
		//      model) the S4 royalty (PR2) needs. Same dual-consent gate as (3).
		//      Best-effort; never fails the request.
		if pooled != nil && strings.TrimSpace(sav.VisionModel) != "" && d.poolGate.DecidePoolableOnWrite(ctx, wsID) {
			if b, mErr := distill.MarshalCachedOCR(res, sav); mErr == nil {
				_ = pooled.SetWithOwner(ctx, distillPoolMarker+hash, distill.OCRCacheVersion(sav.VisionModel), wsID, b)
			}
		}
	}
	return res.Markdown, vs, nil, true // private path → no cross-tenant attribution
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
