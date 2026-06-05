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
	cacheVer := ConverterVersion + ":" + string(normalizeTier(tier))
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

	// Vision-OCR fallback: a text-less document + a configured dispatcher → OCR
	// it. visionFallback records the cost as a COST (Savings.VisionTokensCost,
	// TokensSaved=0) and degrades gracefully (stays NeedsVision) on failure. The
	// OCR result is NOT cached in this stage (OCR caching is deferred), so we
	// return before the cache store.
	if vision != nil && res.NeedsVision {
		vr, vsav := visionFallback(ctx, input, res, vision)
		return vr, vsav, nil
	}

	// Cache only a real, usable conversion — never a NeedsVision/empty result.
	if cache != nil && !res.NeedsVision && res.Markdown != "" {
		if b, mErr := json.Marshal(cachedResult{Result: res}); mErr == nil {
			_ = cache.Set(ctx, hash, cacheVer, b) // best-effort; never fail the conversion
		}
	}

	return res, computeSavings(input, res, false), nil
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
