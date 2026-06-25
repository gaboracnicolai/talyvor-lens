package proxy

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/distill"
)

// PR1 — cross-tenant OCR pool privacy proofs. A pooled OCR serve (one tenant's
// scanned-document transcription served to another — the most sensitive distill
// disclosure) happens ONLY under full dual consent: the global switch AND the
// owner's opt-in AND the requester's opt-in. These reuse the SAME cache_pooling
// gate the conversion pool uses (newScopedDistiller wires LENS_DISTILL_POOLABLE_ENABLED
// + per-workspace distill_poolable) over a REAL miniredis cache — not a skip — and
// the OCR planner tags each transcription with its dispatch number so a leak is
// visible in the served bytes.

// ocrPoolEntry reads (value, owner) for a pooled OCR entry via the same
// ownerDistillCache the integration uses — to assert the owner stamp directly.
func ocrPoolEntry(t *testing.T, d *distillIntegration, content, model string) (value []byte, owner string) {
	t.Helper()
	pooled, ok := d.cache.(ownerDistillCache)
	if !ok {
		t.Fatal("test cache is not an ownerDistillCache")
	}
	b, o, err := pooled.GetWithOwner(context.Background(),
		distill.PoolMarker+distill.ContentHash([]byte(content)), distill.OCRCacheVersion(model))
	if err != nil {
		t.Fatalf("GetWithOwner: %v", err)
	}
	return b, o
}

// TestOCRPool_ConsentedServe_ByteIdentical_OwnerStamped — the positive proof:
// global on + owner AND requester opted in → tenant B is served tenant A's cached
// OCR WITHOUT re-dispatching vision, byte-identical to what A produced, and the
// pooled entry is stamped owner=A (≠ B). Also proves the OCR result is NOT leaked
// into the conversion keyspace (which read (1) would otherwise shadow-serve).
func TestOCRPool_ConsentedServe_ByteIdentical_OwnerStamped(t *testing.T) {
	d := newScopedDistiller(t, needsVisionConv{}, true, map[string]bool{"wsA": true, "wsB": true})
	vis := &ocrPlanner{model: "m"}
	ctx := context.Background()
	const content = "shared-scanned-doc"
	doc := docBlockBytes(content)

	// A OCRs the doc (dispatch #1, real spend) and publishes to the OCR pool.
	mdA, vsA, _, okA := d.tryConvertBlock(ctx, doc, vis, "wsA")
	if !okA {
		t.Fatal("wsA: not ok")
	}
	if vis.calls != 1 || !vsA.recorded() {
		t.Fatalf("wsA first OCR must dispatch + book spend: calls=%d recorded=%v", vis.calls, vsA.recorded())
	}

	// B sends the SAME bytes — served A's pooled OCR, NO re-dispatch, NO new spend.
	mdB, vsB, _, okB := d.tryConvertBlock(ctx, doc, vis, "wsB")
	if !okB {
		t.Fatal("wsB: not ok")
	}
	if vis.calls != 1 {
		t.Fatalf("consented cross-tenant OCR serve must NOT re-dispatch vision: calls=%d, want 1", vis.calls)
	}
	if mdB != mdA {
		t.Fatalf("B's served OCR must be byte-identical to A's cached OCR: %q vs %q", mdB, mdA)
	}
	if vsB.recorded() {
		t.Error("a cross-tenant OCR cache hit must book NO new vision spend for B")
	}

	// Owner stamp: the pooled OCR entry is A's, not B's.
	if _, owner := ocrPoolEntry(t, d, content, "m"); owner != "wsA" {
		t.Errorf("pooled OCR owner stamp = %q, want wsA", owner)
	}
	// The OCR result must live ONLY in the OCR keyspace — never the conversion
	// keyspace — so the conversion pooled read (1) can't shadow the OCR pool with a
	// cost-basis-less copy (the basis the S4 royalty/PR2 needs).
	pooled, _ := d.cache.(ownerDistillCache)
	if b, _, _ := pooled.GetWithOwner(ctx,
		distill.PoolMarker+distill.ContentHash([]byte(content)),
		distill.CacheVersion(distill.TierFaithful)); len(b) > 0 {
		t.Error("OCR result leaked into the conversion pooled keyspace; must be OCR-keyspace-only")
	}
}

// TestOCRPool_OwnerOptedOut_NoServe — fail-closed (owner side): the owner's opt-in
// is checked at SERVE time, so if A opts out after publishing, B is no longer
// served A's OCR and re-dispatches.
func TestOCRPool_OwnerOptedOut_NoServe(t *testing.T) {
	poolable := map[string]bool{"wsA": true, "wsB": true}
	d := newScopedDistiller(t, needsVisionConv{}, true, poolable)
	vis := &ocrPlanner{model: "m"}
	ctx := context.Background()
	doc := docBlockBytes("doc")

	_, _, _, _ = d.tryConvertBlock(ctx, doc, vis, "wsA") // A publishes (dispatch #1)
	poolable["wsA"] = false                              // A revokes consent
	if _, _, _, ok := d.tryConvertBlock(ctx, doc, vis, "wsB"); !ok {
		t.Fatal("wsB: not ok")
	}
	if vis.calls != 2 {
		t.Fatalf("owner opt-out must deny the pooled OCR serve → B re-dispatches: calls=%d, want 2", vis.calls)
	}
}

// TestOCRPool_RequesterNotOptedIn_NoServe — fail-closed (requester side): global on,
// owner opted in, requester NOT opted in → no cross-tenant OCR serve (re-dispatch).
func TestOCRPool_RequesterNotOptedIn_NoServe(t *testing.T) {
	d := newScopedDistiller(t, needsVisionConv{}, true, map[string]bool{"wsA": true}) // wsB absent → false
	vis := &ocrPlanner{model: "m"}
	ctx := context.Background()
	doc := docBlockBytes("doc")

	_, _, _, _ = d.tryConvertBlock(ctx, doc, vis, "wsA")
	_, _, _, _ = d.tryConvertBlock(ctx, doc, vis, "wsB")
	if vis.calls != 2 {
		t.Fatalf("a non-opted-in requester must not be served pooled OCR → re-dispatch: calls=%d, want 2", vis.calls)
	}
}

// TestOCRPool_GlobalFlagOff_BothOptedIn_NoServe — the strong no-regression guard:
// BOTH opted in, but the global switch is OFF (the default) → strictly private OCR,
// EXACTLY today's behavior (B re-dispatches). This isolates LENS_DISTILL_POOLABLE_ENABLED
// as a hard gate: an accidental pool consult would serve A's OCR and fail here.
func TestOCRPool_GlobalFlagOff_BothOptedIn_NoServe(t *testing.T) {
	d := newScopedDistiller(t, needsVisionConv{}, false, map[string]bool{"wsA": true, "wsB": true})
	vis := &ocrPlanner{model: "m"}
	ctx := context.Background()
	doc := docBlockBytes("doc")

	mdA, _, _, _ := d.tryConvertBlock(ctx, doc, vis, "wsA")
	mdB, _, _, okB := d.tryConvertBlock(ctx, doc, vis, "wsB")
	if !okB {
		t.Fatal("wsB: not ok")
	}
	if vis.calls != 2 {
		t.Fatalf("global flag off must keep OCR strictly private → B re-dispatches: calls=%d, want 2", vis.calls)
	}
	if mdB == mdA {
		t.Fatal("LEAK: global flag off but B was served A's OCR transcription")
	}
}

// TestOCRPool_NonPlannerVision_NoPool — fail-safe: a vision dispatcher that cannot
// plan a model (not a distill.ModelPlanner) skips OCR pooling entirely, even under
// full consent → no cross-tenant serve (B re-dispatches). Mirrors orchestrateVision's
// canCache=false guard, so the pool can never serve under an unknown model.
func TestOCRPool_NonPlannerVision_NoPool(t *testing.T) {
	d := newScopedDistiller(t, needsVisionConv{}, true, map[string]bool{"wsA": true, "wsB": true})
	vis := &nonPlannerVision{}
	ctx := context.Background()
	doc := docBlockBytes("doc")

	_, _, _, _ = d.tryConvertBlock(ctx, doc, vis, "wsA")
	_, _, _, _ = d.tryConvertBlock(ctx, doc, vis, "wsB")
	if vis.calls != 2 {
		t.Fatalf("a non-ModelPlanner vision dispatcher must skip OCR pooling → re-dispatch: calls=%d, want 2", vis.calls)
	}
}

// nonPlannerVision is a VisionDispatcher that is deliberately NOT a ModelPlanner.
type nonPlannerVision struct{ calls int }

func (v *nonPlannerVision) DispatchVision(_ context.Context, _ distill.VisionRequest) (distill.VisionResult, error) {
	v.calls++
	return distill.VisionResult{Markdown: "ocr-text", InputTokens: 100, OutputTokens: 10, Model: "m"}, nil
}
