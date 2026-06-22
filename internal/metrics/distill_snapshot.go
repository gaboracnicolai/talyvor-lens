package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// DistillStats is a point-in-time snapshot of the DISTILL counters, read back
// out of Prometheus for the dashboard panel. All token values are the same
// len/4 basis the gateway bills spend on, so TokensSaved and VisionTokensCost
// are directly comparable (a net subtraction is valid). The dashboard is the
// only reader; nothing here mutates the counters.
type DistillStats struct {
	TokensSaved      float64 // lens_distill_tokens_saved_total (a real saving)
	VisionTokensCost float64 // lens_distill_vision_tokens_cost_total (a real COST, never a saving)
	CacheHits        float64 // lens_distill_cache_total{result="hit",kind="conversion"}
	CacheMisses      float64 // lens_distill_cache_total{result="miss",kind="conversion"}
	OCRCacheHits     float64 // lens_distill_cache_total{result="hit",kind="ocr"}
	OCRCacheMisses   float64 // lens_distill_cache_total{result="miss",kind="ocr"}
}

// DistillSnapshot reads the current DISTILL counter values. Reading the cache
// children with WithLabelValues lazily creates a zero counter if one has not
// been observed yet — harmless, and it keeps the snapshot total even before the
// first hit/miss.
func DistillSnapshot() DistillStats {
	return DistillStats{
		TokensSaved:      counterValue(DistillTokensSavedTotal),
		VisionTokensCost: counterValue(DistillVisionTokensCostTotal),
		CacheHits:        counterValue(DistillCacheTotal.WithLabelValues("hit", "conversion")),
		CacheMisses:      counterValue(DistillCacheTotal.WithLabelValues("miss", "conversion")),
		OCRCacheHits:     counterValue(DistillCacheTotal.WithLabelValues("hit", "ocr")),
		OCRCacheMisses:   counterValue(DistillCacheTotal.WithLabelValues("miss", "ocr")),
	}
}

// counterValue reads a counter's current value via the client's Write API — the
// official in-process readback (no testutil dependency, no duplicate state). A
// write error (never expected for a plain counter) yields 0.
func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}
