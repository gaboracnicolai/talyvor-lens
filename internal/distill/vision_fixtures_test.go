package distill

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestVisionFixtures_AreTextLess is a KEYLESS CI GUARD for the vision-OCR
// fidelity benchmark (see internal/proxy/vision_benchmark_test.go). It pins the
// load-bearing property of every committed fixture under testdata/vision/: the
// PDF must be TEXT-LESS, so pdf.go routes it to NeedsVision and a vision model
// must genuinely OCR the pixels. If a fixture ever grew an extractable text layer,
// the benchmark would silently measure text-layer reading instead of OCR — a
// false high-fidelity result. Deterministic, free, no API key: runs in CI.
func TestVisionFixtures_AreTextLess(t *testing.T) {
	dir := filepath.Join("testdata", "vision")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixtures dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".pdf" {
			continue
		}
		n++
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		// A text-less / image-only / unparseable PDF routes to NeedsVision
		// (pdf.go: empty text OR a recovered parse panic). Either way, NOT a
		// usable text layer — which is exactly the property we require.
		res, err := DistillAs(context.Background(), b, FormatPDF)
		if err != nil {
			t.Errorf("%s: DistillAs returned error %v", e.Name(), err)
			continue
		}
		if !res.NeedsVision {
			t.Errorf("%s: NOT text-less — pdf.go extracted a text layer (markdown %d bytes: %q). The benchmark would read text, not OCR pixels.",
				e.Name(), len(res.Markdown), res.Markdown)
		}
	}
	if n == 0 {
		t.Fatal("no .pdf fixtures found under testdata/vision — the benchmark has nothing to measure")
	}
	t.Logf("text-less guard: %d vision fixtures confirmed image-only (NeedsVision)", n)
}
