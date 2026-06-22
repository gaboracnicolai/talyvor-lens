package proxy

import (
	"context"
	"fmt"
	"testing"

	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/metrics"
)

// needsVisionConv yields a text-less (NeedsVision) result, so a document routes to
// the OCR fallback instead of plain conversion.
type needsVisionConv struct{}

func (needsVisionConv) Convert(_ context.Context, _ []byte, format distill.Format) (distill.Result, error) {
	return distill.Result{NeedsVision: true, Format: format}, nil
}

// ocrPlanner is a proxy-side vision dispatcher + distill.ModelPlanner that counts
// dispatches and tags each transcription with its call number, so a cross-tenant
// leak (one tenant served another's cached OCR) is visible in the bytes.
type ocrPlanner struct {
	calls int
	model string
}

func (p *ocrPlanner) DispatchVision(_ context.Context, _ distill.VisionRequest) (distill.VisionResult, error) {
	p.calls++
	return distill.VisionResult{
		Markdown:     fmt.Sprintf("OCR#%d by %s", p.calls, p.model),
		InputTokens:  500,
		OutputTokens: 20,
		Model:        p.model,
	}, nil
}

func (p *ocrPlanner) PlannedVisionModel(_ context.Context) (string, bool) { return p.model, true }

// TestOCRCache_PrivateByDefault_NoCrossTenantServe — the privacy guard, mirroring
// S0: tenant A OCRs a scanned doc; tenant B submitting the SAME bytes is NOT served
// A's OCR (the wsID-scoped key) and re-dispatches; A re-submitting hits its OWN OCR
// cache (no new dispatch, no new spend). Also asserts the distinct kind="ocr" metric.
func TestOCRCache_PrivateByDefault_NoCrossTenantServe(t *testing.T) {
	d := newScopedDistiller(t, needsVisionConv{}, false, nil) // pooling fully off (default)
	vis := &ocrPlanner{model: "m"}
	ctx := context.Background()
	doc := docBlockBytes("a-scanned-document")
	base := metrics.DistillSnapshot()

	// A OCRs the doc (dispatch #1, real vision spend booked).
	mdA, vsA, _, okA := d.tryConvertBlock(ctx, doc, vis, "wsA")
	if !okA {
		t.Fatal("wsA: not ok")
	}
	if vis.calls != 1 {
		t.Fatalf("wsA OCR dispatches = %d, want 1", vis.calls)
	}
	if !vsA.recorded() {
		t.Error("wsA first OCR must book a vision spend")
	}

	// B sends the SAME bytes — must NOT be served A's OCR; it re-dispatches (#2).
	mdB, _, _, okB := d.tryConvertBlock(ctx, doc, vis, "wsB")
	if !okB {
		t.Fatal("wsB: not ok")
	}
	if vis.calls != 2 {
		t.Fatalf("cross-tenant: dispatches = %d, want 2 (B must NOT be served A's OCR)", vis.calls)
	}
	if mdB == mdA {
		t.Fatal("LEAK: wsB was served wsA's cached OCR transcription")
	}

	// A re-submits the SAME bytes — hits its OWN OCR cache (no new dispatch, no spend).
	mdA2, vsA2, _, _ := d.tryConvertBlock(ctx, doc, vis, "wsA")
	if vis.calls != 2 {
		t.Fatalf("wsA re-serve must HIT the OCR cache: dispatches = %d, want 2", vis.calls)
	}
	if mdA2 != mdA {
		t.Errorf("wsA OCR cache hit not byte-identical: %q vs %q", mdA2, mdA)
	}
	if vsA2.recorded() {
		t.Error("an OCR cache hit must book NO new vision spend")
	}

	// The kind="ocr" hit/miss is distinctly observable (A hit; A+B misses).
	snap := metrics.DistillSnapshot()
	if d := snap.OCRCacheHits - base.OCRCacheHits; d != 1 {
		t.Errorf("OCRCacheHits delta = %v, want 1", d)
	}
	if d := snap.OCRCacheMisses - base.OCRCacheMisses; d < 2 {
		t.Errorf("OCRCacheMisses delta = %v, want >=2 (wsA miss + wsB miss)", d)
	}
}
