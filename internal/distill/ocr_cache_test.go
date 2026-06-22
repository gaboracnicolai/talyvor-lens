package distill

import (
	"context"
	"strings"
	"testing"
)

// plannerMock is a VisionDispatcher that ALSO implements ModelPlanner: it reports
// a planned model (so the orchestrator's OCR cache engages) and counts dispatches,
// so a test can prove a cache HIT did NOT re-invoke the vision model. Its OCR text
// embeds the model, so a wrong-model serve is detectable in the bytes.
type plannerMock struct {
	calls  int
	model  string // planned + served model
	planOK bool   // false → not cacheable (fail-safe path)
	fail   bool   // true → OCR yields no text (graceful failure)
}

func (m *plannerMock) DispatchVision(_ context.Context, _ VisionRequest) (VisionResult, error) {
	m.calls++
	if m.fail {
		return VisionResult{}, nil // empty → visionFallback stays NeedsVision
	}
	return VisionResult{
		Markdown:     "# OCR by " + m.model + "\n\nrecovered text",
		InputTokens:  1000,
		OutputTokens: 40,
		Model:        m.model,
	}, nil
}

func (m *plannerMock) PlannedVisionModel(_ context.Context) (string, bool) {
	return m.model, m.planOK
}

// scannedConv is a converter that yields a text-less (NeedsVision) result — the
// document that triggers the OCR fallback.
func scannedConv() *fakeConv { return &fakeConv{res: Result{NeedsVision: true}} }

var scannedDoc = []byte("%PDF-1.5 scanned-image-only document")

// TestOCRCache_HitSkipsDispatch — the core proof. A re-submitted scanned doc hits
// the OCR cache: the dispatcher is NOT re-invoked, the served OCR is byte-identical,
// and the hit books NO new vision cost.
func TestOCRCache_HitSkipsDispatch(t *testing.T) {
	ctx := context.Background()
	c := &fakeCache{}
	vis := &plannerMock{model: "claude-3-5-sonnet", planOK: true}

	r1, s1, err := Orchestrate(ctx, scannedConv(), c, vis, scannedDoc, FormatPDF, TierFaithful)
	if err != nil {
		t.Fatal(err)
	}
	if vis.calls != 1 {
		t.Fatalf("first submit: dispatches = %d, want 1", vis.calls)
	}
	if s1.CacheHit {
		t.Error("first submit must be a miss (CacheHit=false)")
	}
	if s1.VisionTokensCost == 0 {
		t.Error("first OCR must book a real vision cost")
	}

	r2, s2, err := Orchestrate(ctx, scannedConv(), c, vis, scannedDoc, FormatPDF, TierFaithful)
	if err != nil {
		t.Fatal(err)
	}
	if vis.calls != 1 {
		t.Fatalf("second submit: dispatches = %d, want 1 (OCR cache HIT must not re-dispatch)", vis.calls)
	}
	if !s2.CacheHit {
		t.Error("second submit must be a cache hit (CacheHit=true)")
	}
	if s2.VisionTokensCost != 0 {
		t.Errorf("cache hit must book NO new vision cost, got %d", s2.VisionTokensCost)
	}
	if r2.Markdown != r1.Markdown {
		t.Errorf("served OCR not byte-identical: %q vs %q", r2.Markdown, r1.Markdown)
	}
	if r2.Method != MethodVisionOCR {
		t.Error("served cached result must retain Method=vision_ocr provenance")
	}
}

// TestOCRCache_DifferentBytesMiss — a DIFFERENT document (different bytes) does NOT
// hit a prior entry (no wrong-document serve).
func TestOCRCache_DifferentBytesMiss(t *testing.T) {
	ctx := context.Background()
	c := &fakeCache{}
	vis := &plannerMock{model: "m", planOK: true}

	Orchestrate(ctx, scannedConv(), c, vis, []byte("doc-AAAA"), FormatPDF, TierFaithful)
	Orchestrate(ctx, scannedConv(), c, vis, []byte("doc-BBBB different bytes"), FormatPDF, TierFaithful)
	if vis.calls != 2 {
		t.Fatalf("different bytes: dispatches = %d, want 2 (a different document must re-OCR)", vis.calls)
	}
}

// TestOCRCache_DifferentModelMiss — the isolation test: SAME document bytes,
// DIFFERENT planned model → MISS → re-OCR under the new model. Holds bytes constant
// and varies ONLY the model, proving the model is actually IN the key (a workspace
// changing its allow-list re-OCRs instead of serving the prior model's transcription).
func TestOCRCache_DifferentModelMiss(t *testing.T) {
	ctx := context.Background()
	c := &fakeCache{}

	visA := &plannerMock{model: "model-A", planOK: true}
	r1, _, _ := Orchestrate(ctx, scannedConv(), c, visA, scannedDoc, FormatPDF, TierFaithful)
	if visA.calls != 1 {
		t.Fatalf("A dispatches = %d, want 1", visA.calls)
	}

	visB := &plannerMock{model: "model-B", planOK: true}
	r2, _, _ := Orchestrate(ctx, scannedConv(), c, visB, scannedDoc, FormatPDF, TierFaithful) // SAME bytes
	if visB.calls != 1 {
		t.Fatalf("wrong-model serve: B dispatches = %d, want 1 (a different model MUST re-OCR)", visB.calls)
	}
	if r2.Markdown == r1.Markdown {
		t.Fatal("different model served the prior model's cached transcription — WRONG-MODEL SERVE")
	}
	if !strings.Contains(r2.Markdown, "model-B") {
		t.Errorf("served text is not model B's OCR: %q", r2.Markdown)
	}
}

// TestOCRCache_NoPlannerNoCache — a dispatcher that is NOT a ModelPlanner: the cache
// is skipped, every submit dispatches, nothing is written, no error (fail-safe).
func TestOCRCache_NoPlannerNoCache(t *testing.T) {
	ctx := context.Background()
	c := &fakeCache{}
	vis := &mockVisionDispatcher{res: VisionResult{Markdown: "# ocr", Model: "m", InputTokens: 10, OutputTokens: 5}}

	Orchestrate(ctx, scannedConv(), c, vis, scannedDoc, FormatPDF, TierFaithful)
	Orchestrate(ctx, scannedConv(), c, vis, scannedDoc, FormatPDF, TierFaithful)
	if vis.calls != 2 {
		t.Fatalf("no-planner: dispatches = %d, want 2 (no OCR caching)", vis.calls)
	}
	if c.sets != 0 {
		t.Errorf("no-planner path must not write the OCR cache, got %d sets", c.sets)
	}
}

// TestOCRCache_PlanNotOKNoCache — a ModelPlanner that returns ok=false (no capable
// model) likewise skips the cache (fail-safe, never a wrong-model serve).
func TestOCRCache_PlanNotOKNoCache(t *testing.T) {
	ctx := context.Background()
	c := &fakeCache{}
	vis := &plannerMock{model: "", planOK: false}

	Orchestrate(ctx, scannedConv(), c, vis, scannedDoc, FormatPDF, TierFaithful)
	Orchestrate(ctx, scannedConv(), c, vis, scannedDoc, FormatPDF, TierFaithful)
	if vis.calls != 2 {
		t.Fatalf("plan-not-ok: dispatches = %d, want 2", vis.calls)
	}
	if c.sets != 0 {
		t.Errorf("plan-not-ok path must not write the OCR cache, got %d sets", c.sets)
	}
}

// TestOCRCache_FailureNotCached — a graceful OCR FAILURE (stays NeedsVision) is never
// cached, so a retry re-dispatches.
func TestOCRCache_FailureNotCached(t *testing.T) {
	ctx := context.Background()
	c := &fakeCache{}
	vis := &plannerMock{model: "m", planOK: true, fail: true}

	r, _, _ := Orchestrate(ctx, scannedConv(), c, vis, scannedDoc, FormatPDF, TierFaithful)
	if !r.NeedsVision {
		t.Error("a failed OCR must stay NeedsVision")
	}
	Orchestrate(ctx, scannedConv(), c, vis, scannedDoc, FormatPDF, TierFaithful)
	if vis.calls != 2 {
		t.Fatalf("failed OCR must not be cached: dispatches = %d, want 2", vis.calls)
	}
	if c.sets != 0 {
		t.Errorf("a failed OCR must not be written, got %d sets", c.sets)
	}
}
