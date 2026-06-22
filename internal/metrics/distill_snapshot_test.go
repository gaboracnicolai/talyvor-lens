package metrics

import "testing"

// DistillSnapshot must read the live DISTILL counter values back out (for the
// dashboard panel), using deltas so it's robust to whatever other tests in the
// process already incremented.
func TestDistillSnapshot_ReadsCounters(t *testing.T) {
	base := DistillSnapshot()

	DistillTokensSaved(100)
	DistillVisionTokensCost(30)
	DistillCache("hit", "conversion")
	DistillCache("hit", "conversion")
	DistillCache("miss", "conversion")
	DistillCache("hit", "ocr")
	DistillCache("miss", "ocr")
	DistillCache("miss", "ocr")

	got := DistillSnapshot()

	if d := got.TokensSaved - base.TokensSaved; d != 100 {
		t.Errorf("TokensSaved delta = %v, want 100", d)
	}
	if d := got.VisionTokensCost - base.VisionTokensCost; d != 30 {
		t.Errorf("VisionTokensCost delta = %v, want 30", d)
	}
	if d := got.CacheHits - base.CacheHits; d != 2 {
		t.Errorf("CacheHits (conversion) delta = %v, want 2", d)
	}
	if d := got.CacheMisses - base.CacheMisses; d != 1 {
		t.Errorf("CacheMisses (conversion) delta = %v, want 1", d)
	}
	if d := got.OCRCacheHits - base.OCRCacheHits; d != 1 {
		t.Errorf("OCRCacheHits delta = %v, want 1", d)
	}
	if d := got.OCRCacheMisses - base.OCRCacheMisses; d != 2 {
		t.Errorf("OCRCacheMisses delta = %v, want 2", d)
	}
}
