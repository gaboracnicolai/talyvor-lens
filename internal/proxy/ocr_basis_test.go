package proxy

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
)

// L2/S4 PR2 — avoided-COGS basis capture at the cross-tenant OCR serve site. These
// prove the FIGURE (captured at serve time from A's cached OCR) and which events
// record it. miniredis = real cache semantics (the cross-tenant serve is genuine);
// the durable PG persistence + idempotency are pinned in distillattrib/basis_test.go.

// TestOCRPool_ConsentedServe_CapturesBasisFaithfully — a consented cross-tenant OCR
// serve surfaces a fact whose avoided-COGS EXACTLY equals CostUSD(cached model, in,
// out), with provenance (model + token split) faithful to the cached entry. So
// recomputing CostUSD(stored provenance) yields the recorded figure — not just a
// bare non-null number.
func TestOCRPool_ConsentedServe_CapturesBasisFaithfully(t *testing.T) {
	d := newScopedDistiller(t, needsVisionConv{}, true, map[string]bool{"wsA": true, "wsB": true})
	// A real catalog model so CostUSD > 0 (a non-vacuous basis). ocrPlanner books
	// 500 input / 20 output tokens (see its DispatchVision).
	vis := &ocrPlanner{model: "gpt-4o-mini"}
	ctx := context.Background()
	doc := docBlockBytes("scanned-for-basis")

	if _, _, _, ok := d.tryConvertBlock(ctx, doc, vis, "wsA"); !ok { // A OCRs + pools (owner=wsA)
		t.Fatal("wsA: not ok")
	}
	_, _, fact, ok := d.tryConvertBlock(ctx, doc, vis, "wsB") // B served A's pooled OCR
	if !ok || fact == nil {
		t.Fatalf("consented cross-tenant OCR serve must emit a fact: ok=%v fact=%v", ok, fact)
	}
	if fact.owner != "wsA" {
		t.Fatalf("fact.owner = %q, want wsA", fact.owner)
	}
	// Provenance EXACTLY faithful to the cached entry (A's actual vision call).
	if fact.visionModel != "gpt-4o-mini" || fact.visionInputTokens != 500 || fact.visionOutputTokens != 20 {
		t.Fatalf("provenance = (%s,%d,%d), want (gpt-4o-mini,500,20)",
			fact.visionModel, fact.visionInputTokens, fact.visionOutputTokens)
	}
	// The number EXACTLY = CostUSD(cached model, in, out), and non-vacuous (> 0).
	want := alerts.CostUSD("gpt-4o-mini", 500, 20)
	if want <= 0 {
		t.Fatalf("precondition: CostUSD must be > 0 for a non-vacuous proof, got %v", want)
	}
	if fact.avoidedCOGSUSD != want {
		t.Fatalf("avoidedCOGSUSD = %v, want exactly CostUSD(gpt-4o-mini,500,20) = %v", fact.avoidedCOGSUSD, want)
	}
	// Re-derivable from the recorded provenance.
	if alerts.CostUSD(fact.visionModel, fact.visionInputTokens, fact.visionOutputTokens) != fact.avoidedCOGSUSD {
		t.Fatal("recorded basis is not re-derivable from its recorded provenance")
	}
}

// TestOCRPool_SelfServe_NoBasis — same-tenant OCR reuse (owner == requester) emits NO
// fact, so NO basis is captured (a workspace avoids nothing cross-tenant by reusing
// its own OCR).
func TestOCRPool_SelfServe_NoBasis(t *testing.T) {
	d := newScopedDistiller(t, needsVisionConv{}, true, map[string]bool{"wsA": true})
	vis := &ocrPlanner{model: "gpt-4o-mini"}
	ctx := context.Background()
	doc := docBlockBytes("self-ocr")

	_, _, _, _ = d.tryConvertBlock(ctx, doc, vis, "wsA")      // produce + pool (owner=wsA)
	_, _, fact, ok := d.tryConvertBlock(ctx, doc, vis, "wsA") // self-serve from the pool
	if !ok {
		t.Fatal("wsA self-serve: not ok")
	}
	if fact != nil {
		t.Fatalf("self-serve emitted a fact (would capture a basis): %+v", *fact)
	}
}

// TestConversionServe_NoBasisFields — a consented cross-tenant CONVERSION serve emits
// a fact (for serve_count) but with NO basis (visionModel empty / cogs zero), so
// recordDistillServes records no avoided-COGS for it. Only genuine OCR reuse has an
// avoided vision cost.
func TestConversionServe_NoBasisFields(t *testing.T) {
	d := newScopedDistiller(t, &countingConv{}, true, map[string]bool{"wsA": true, "wsB": true})
	ctx := context.Background()
	doc := docBlockBytes("shared-conv")

	_, _, _, _ = d.tryConvertBlock(ctx, doc, nil, "wsA") // produce + pool (conversion)
	_, _, fact, ok := d.tryConvertBlock(ctx, doc, nil, "wsB")
	if !ok || fact == nil {
		t.Fatalf("conversion cross-tenant serve must emit a fact: ok=%v fact=%v", ok, fact)
	}
	if fact.visionModel != "" || fact.avoidedCOGSUSD != 0 {
		t.Fatalf("a conversion fact must carry NO basis, got model=%q cogs=%v", fact.visionModel, fact.avoidedCOGSUSD)
	}
}
