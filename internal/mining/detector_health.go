// detector_health.go — Phase-4a Item 4: the detector-health signal. The
// settlement fail-closed layer withholds settlement from any un-examined held
// mint, so a stalled/crashed detector STRANDS legitimate mints (recoverable,
// deflationary). That trade is acceptable ONLY if the stall is VISIBLE — an
// operator must be able to alert on "the detector hasn't run". This tracker
// heartbeats each clearer/detector RunOnce and reports healthy/stale; main
// surfaces it on a health endpoint + a Prometheus gauge.
package mining

import (
	"sync/atomic"
	"time"

	"github.com/talyvor/lens/internal/metrics"
)

// DetectorHealth tracks the last successful run of a detector/clearer and whether
// it is stale. Safe for concurrent use. The zero value is not usable — use
// NewDetectorHealth.
type DetectorHealth struct {
	name     string
	maxStale time.Duration
	lastNano atomic.Int64 // unix nanos of the last MarkRun; 0 = never run
}

// NewDetectorHealth builds a health tracker. maxStale is how long since the last
// run before the detector is considered unhealthy (non-positive ⇒ 15m). A
// detector should MarkRun each successful sweep; the caller sets maxStale to a
// small multiple of the sweep interval.
func NewDetectorHealth(name string, maxStale time.Duration) *DetectorHealth {
	if maxStale <= 0 {
		maxStale = 15 * time.Minute
	}
	return &DetectorHealth{name: name, maxStale: maxStale}
}

// MarkRun records a successful run now and updates the freshness gauge.
func (h *DetectorHealth) MarkRun() { h.markRunAt(time.Now()) }

func (h *DetectorHealth) markRunAt(t time.Time) {
	if h == nil {
		return
	}
	h.lastNano.Store(t.UnixNano())
	metrics.SetDetectorLastRunAgeSeconds(h.name, 0)
}

// LastRun is the time of the last successful run (zero if never).
func (h *DetectorHealth) LastRun() time.Time {
	if h == nil {
		return time.Time{}
	}
	n := h.lastNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// StaleFor is how long since the last run (a large sentinel if never run).
func (h *DetectorHealth) StaleFor() time.Duration {
	last := h.LastRun()
	if last.IsZero() {
		return 100 * 365 * 24 * time.Hour // "forever" — never ran
	}
	return time.Since(last)
}

// Healthy reports whether the detector ran within maxStale. A never-run detector
// is unhealthy (fail-visible: we do not assume health we haven't observed).
func (h *DetectorHealth) Healthy() bool {
	if h == nil {
		return false
	}
	if h.LastRun().IsZero() {
		return false
	}
	return h.StaleFor() <= h.maxStale
}

// Name / MaxStale expose the tracker's identity + threshold for the health handler.
func (h *DetectorHealth) Name() string            { return h.name }
func (h *DetectorHealth) MaxStale() time.Duration { return h.maxStale }

// PublishAge sets the freshness gauge to the current staleness — call on a tick so
// a stall is visible in metrics even between runs.
func (h *DetectorHealth) PublishAge() {
	if h == nil {
		return
	}
	metrics.SetDetectorLastRunAgeSeconds(h.name, h.StaleFor().Seconds())
}
