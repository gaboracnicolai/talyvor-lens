package distill

import (
	"context"
	"encoding/json"
	"strings"
)

// IsolatedConverter is the conversion seam the orchestrator drives: a killable
// subprocess that converts untrusted document bytes out-of-process. It is
// satisfied by *ProcessIsolator (and by a fake in tests). The orchestrator
// NEVER calls the in-process Distill/DistillAs entrypoints — untrusted bytes go
// only through this interface.
type IsolatedConverter interface {
	Convert(ctx context.Context, input []byte, format Format) (Result, error)
}

// Orchestrate is the reusable "convert one document end-to-end" entry the
// request path (and the admin preview) use: isolated Convert → parent-side
// ApplyTier → optional content-addressed cache → honest Savings. It is the
// generalised form of the preview's logic.
//
// cache is optional (nil disables it). vision is the OPTIONAL live vision-OCR
// dispatcher (nil = no vision): when the isolated conversion yields a
// NeedsVision result (scanned/text-less), the document is OCR'd via vision and
// the cost is accounted HONESTLY (a cost, never a saving). A conversion error is
// returned to the caller (which, on the request path, passes the original
// request through untouched — distillation never fails a user's request); a
// VISION failure is NOT an error — the result stays NeedsVision (graceful).
func Orchestrate(ctx context.Context, conv IsolatedConverter, cache Cache, vision VisionDispatcher, input []byte, format Format, tier Tier) (Result, Savings, error) {
	// Key the cache on content + converter version + tier (colon-free segments
	// preserve key injectivity), exactly like DistillWithCache.
	cacheVer := CacheVersion(tier)
	hash := ContentHash(input)

	if cache != nil {
		if b, err := cache.Get(ctx, hash, cacheVer); err == nil && len(b) > 0 {
			var cr cachedResult
			if json.Unmarshal(b, &cr) == nil {
				return cr.Result, computeSavings(input, cr.Result, true), nil
			}
			// Corrupt entry → fall through to a fresh conversion.
		}
	}

	res, err := conv.Convert(ctx, input, format)
	if err != nil {
		return res, Savings{InputBytes: len(input)}, err
	}

	// Parent-side tier on the faithful subprocess output.
	res = applyTier(res, tier)

	// Vision-OCR fallback: a text-less document + a configured dispatcher → OCR it,
	// with a result cache in front so a re-submitted scanned document reuses the
	// prior OCR instead of re-dispatching the expensive vision model.
	if vision != nil && res.NeedsVision {
		return orchestrateVision(ctx, cache, vision, input, hash, res)
	}

	// Cache only a real, usable conversion — never a NeedsVision/empty result.
	if cache != nil && !res.NeedsVision && res.Markdown != "" {
		if b, mErr := json.Marshal(cachedResult{Result: res}); mErr == nil {
			_ = cache.Set(ctx, hash, cacheVer, b) // best-effort; never fail the conversion
		}
	}

	return res, computeSavings(input, res, false), nil
}

// orchestrateVision runs the OCR fallback with a RESULT CACHE in front of it,
// keyed on (bytes, OCRVersion, vision model) in a keyspace DISTINCT from the
// conversion cache (OCRCacheVersion's "ocr:" prefix). A re-submitted scanned
// document reuses the prior OCR instead of re-dispatching the expensive vision
// model. Correctness is the bar:
//   - the MODEL is part of the key, so a workspace changing its model re-OCRs and
//     never serves the old model's transcription;
//   - a dispatcher that cannot report its planned model (not a ModelPlanner, or
//     ok=false: unsupported provider / no capable model) SKIPS the cache and
//     dispatches as before — fail-safe, never a wrong-model serve;
//   - a successful OCR is stored under the ACTUAL model that served it, never a
//     diverging planned model; a graceful FAILURE (stays NeedsVision) is never cached.
func orchestrateVision(ctx context.Context, cache Cache, vision VisionDispatcher, input []byte, hash string, res Result) (Result, Savings, error) {
	planModel, canCache := "", false
	if mp, ok := vision.(ModelPlanner); ok {
		planModel, canCache = mp.PlannedVisionModel(ctx)
	}
	canCache = canCache && cache != nil && planModel != ""

	// LOOKUP (before dispatch): a hit serves the prior OCR — the dispatcher is NOT
	// invoked, and no vision cost is booked (CacheHit=true, VisionTokensCost=0).
	if canCache {
		if b, err := cache.Get(ctx, hash, OCRCacheVersion(planModel)); err == nil && len(b) > 0 {
			if co, ok := UnmarshalCachedOCR(b); ok {
				return co.Result, ocrHitSavings(input, co), nil
			}
			// Corrupt entry → fall through to a fresh OCR.
		}
	}

	vr, vsav := visionFallback(ctx, input, res, vision)

	// STORE only a SUCCESSFUL OCR, under the ACTUAL model that served it (never the
	// planned model, so a plan/dispatch divergence can't file it under the wrong key).
	if canCache && vr.Method == MethodVisionOCR && !vr.NeedsVision && vr.Markdown != "" {
		storeModel := vsav.VisionModel
		if storeModel == "" {
			storeModel = planModel
		}
		if b, err := MarshalCachedOCR(vr, vsav); err == nil {
			_ = cache.Set(ctx, hash, OCRCacheVersion(storeModel), b) // best-effort; never fail the conversion
		}
	}
	return vr, vsav, nil
}

// ocrHitSavings is the Savings for an OCR result served FROM the cache: the OCR'd
// Markdown is delivered, but the vision model was NOT dispatched this time, so the
// cost is ZERO (the value of the cache is exactly the avoided re-OCR). The original
// cost the entry recorded is the AVOIDED cost — kept on the cached value for the S4
// royalty basis, deliberately NOT re-booked as a spend here.
func ocrHitSavings(input []byte, co CachedOCR) Savings {
	return Savings{
		InputTokensRaw:       len(input) / 4,
		InputTokensDistilled: estTokens(co.Result.Markdown),
		TokensSaved:          0, // OCR never "saves" tokens; a cache hit just avoids re-spending.
		VisionTokensCost:     0, // NOT dispatched this time → no new spend booked.
		VisionModel:          co.VisionModel,
		InputBytes:           len(input),
		OutputBytes:          len(co.Result.Markdown),
		CacheHit:             true,
	}
}

// FormatFromMediaType maps a declared chat content-block media type (e.g. an
// Anthropic source.media_type or an OpenAI data-URI MIME) to a distill Format.
// Parameters such as "; charset=utf-8" are ignored; unknown types return false.
// Exported so both the request-path integration and the admin preview share one
// source of truth.
func FormatFromMediaType(mt string) (Format, bool) {
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "application/pdf":
		return FormatPDF, true
	case "text/html", "application/xhtml+xml":
		return FormatHTML, true
	case "text/csv":
		return FormatCSV, true
	case "application/json":
		return FormatJSON, true
	case "text/xml", "application/xml":
		return FormatXML, true
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return FormatDOCX, true
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return FormatXLSX, true
	case "text/plain", "text/markdown":
		return FormatText, true
	}
	return FormatUnknown, false
}
