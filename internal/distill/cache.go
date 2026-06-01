package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/talyvor/lens/internal/metrics"
)

// ConverterVersion identifies the conversion-output contract. BUMP IT whenever
// any converter's Markdown output changes — the conversion cache keys on it, so
// a bump lands on fresh keys and stale Markdown from an old converter is never
// served. (Manual + intentional: a content hash of the converter source would
// be automatic but brittle and noisy; a reviewed version bump is the standard.)
const ConverterVersion = "1"

// Cache is the conversion-cache seam DISTILL depends on. internal/cache's
// DistillCache satisfies it (Redis-backed; same store as the LLM cache, its own
// namespace + metric — not a parallel cache); tests use an in-memory fake. A
// miss returns (nil, nil), never an error.
type Cache interface {
	Get(ctx context.Context, contentHash, version string) ([]byte, error)
	Set(ctx context.Context, contentHash, version string, value []byte) error
}

// Savings is the measured token reduction from one distillation, on the SAME
// len/4 basis the gateway bills spend on (so savings are directly comparable to
// spend — consistency over cleverness).
//
// CAVEAT: InputTokensRaw = len(inputBytes)/4 is only a genuine "prompt cost" for
// text-ish inputs (HTML, CSV, JSON, XML, text). For BINARY formats (DOCX/XLSX/
// PDF) the raw bytes can't be sent to a text model at all, so InputTokensRaw is
// byte-size in the len/4 basis, not a real prompt cost — read InputBytes
// alongside and interpret those savings as "size reduction", not tokens-not-spent.
type Savings struct {
	InputTokensRaw       int  // len(inputBytes)/4
	InputTokensDistilled int  // len(markdown)/4
	TokensSaved          int  // raw - distilled (may be negative; 0 when no Markdown delivered; forced non-positive for vision OCR)
	VisionTokensCost     int  // tokens SPENT by the vision-OCR fallback (a COST, never a saving); 0 unless this Result was OCR'd
	InputBytes           int  // len(inputBytes)
	OutputBytes          int  // len(markdown)
	CacheHit             bool // served from the conversion cache (conversion skipped)
}

// ContentHash is the content-addressed cache key input for a document: sha256
// of the raw bytes, hex-encoded.
func ContentHash(input []byte) string {
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:])
}

// estTokens mirrors the gateway's billing basis exactly: plain len/4, no
// minimum (matching the inline len(prompt)/4 used for spend), so a token saved
// here is the same unit as a token spent elsewhere.
func estTokens(s string) int { return len(s) / 4 }

func computeSavings(input []byte, res Result, cacheHit bool) Savings {
	raw := len(input) / 4
	distilled := estTokens(res.Markdown)
	saved := raw - distilled
	if res.NeedsVision || res.Markdown == "" {
		// No usable Markdown was delivered (text-less PDF, etc.) — distillation
		// saved nothing here; any value comes from the later vision path.
		saved = 0
	}
	return Savings{
		InputTokensRaw:       raw,
		InputTokensDistilled: distilled,
		TokensSaved:          saved,
		InputBytes:           len(input),
		OutputBytes:          len(res.Markdown),
		CacheHit:             cacheHit,
	}
}

// cachedResult is the cache value wire shape (just the Result; Savings are
// recomputed on read since they depend only on input + Markdown).
type cachedResult struct {
	Result Result `json:"result"`
}

// DistillWithCache converts input, using c as a conversion cache (nil disables
// caching). A HIT returns the cached Result without re-converting; a MISS
// converts and stores the Result. Savings are measured every call (recomputed
// from input + Markdown, so they're correct on hits too) and the running
// tokens-saved total is updated. The cache hit/miss metric is recorded by the
// Cache implementation.
//
// This is the reusable piece stage 3 will wire into the request path. It does
// NOT touch token_events or the request path here — it only converts, caches,
// and MEASURES.
func DistillWithCache(ctx context.Context, c Cache, input []byte, opts ...Option) (Result, Savings, error) {
	o := resolveOptions(opts)
	res, sav, err := distillOrCached(ctx, c, input, o, opts)
	if err != nil {
		return res, sav, err
	}

	// Vision-OCR fallback: a text-less document (NeedsVision) + a configured
	// dispatcher → route to a vision model for OCR. This is the EXPENSIVE path —
	// visionFallback records its cost distinctly and NEVER as a saving, so the
	// savings metric below is deliberately skipped here. It runs AFTER the
	// cache: the cache holds the honest text-extraction result; the OCR output
	// is not re-cached in this stage (that joins live dispatch in stage 3), so a
	// NeedsVision cache hit still re-runs the dispatcher.
	if o.vision != nil && res.NeedsVision {
		res, sav = visionFallback(ctx, input, res, o.vision)
		return res, sav, nil
	}

	// Count on hits too: the savings is REALIZED each time distilled Markdown is
	// used in place of the raw doc (per-use value, which stage 3 attaches
	// per-request). Not a unique-conversion count.
	metrics.DistillTokensSaved(sav.TokensSaved)
	return res, sav, nil
}

// distillOrCached returns the conversion Result + Savings, served from c when
// present (nil c disables caching). It records NO metrics — the caller decides,
// because a vision fallback changes the accounting (OCR cost, not a saving).
func distillOrCached(ctx context.Context, c Cache, input []byte, o convOptions, opts []Option) (Result, Savings, error) {
	hash := ContentHash(input)
	// The cache value depends on the TIER (faithful vs outline of the same doc
	// are different outputs), so the tier joins the version in the key:
	// effectively sha256(ConverterVersion : tier : contentHash). Both
	// ConverterVersion and the tier are colon-free constants, preserving the
	// DistillCache.Key injectivity invariant.
	cacheVer := ConverterVersion + ":" + string(normalizeTier(o.tier))

	if c != nil {
		if b, err := c.Get(ctx, hash, cacheVer); err == nil && len(b) > 0 {
			var cr cachedResult
			if json.Unmarshal(b, &cr) == nil {
				return cr.Result, computeSavings(input, cr.Result, true), nil
			}
			// Corrupt cache entry → fall through to a fresh conversion.
		}
	}

	res, err := Distill(ctx, input, opts...)
	if err != nil {
		return res, Savings{InputBytes: len(input)}, err
	}

	if c != nil {
		if b, mErr := json.Marshal(cachedResult{Result: res}); mErr == nil {
			// Best-effort: a cache write failure must never fail the conversion.
			_ = c.Set(ctx, hash, cacheVer, b)
		}
	}
	return res, computeSavings(input, res, false), nil
}
