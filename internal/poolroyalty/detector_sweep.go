// detector_sweep.go — the scheduled "smoke detector": a leader-elected sweep that runs
// the cache + distill fraud detectors on a cadence and RECORDS flagged findings for human
// review. STRUCTURALLY never-auto-act:
//   - it holds ONLY read-only detector seams (sweepDetector/sweepDistillDetector — the same
//     Query/QueryRow-only *DetectorReader/*DistillDetectorReader the /detect endpoints use)
//     and a findingsRecorder (Record-only — no generic Exec, no ledger, no revoke);
//   - this file imports NO ledger (internal/mining) and references no mutation primitive
//     (RevokeHeldTx / CreditHeldTx / Revoker / AdjudicationWriter), so there is no reachable
//     path to a mint/burn/balance change. TestDetectorSweep_NeverActs_ImportGuard pins both.
//
// It only READS the mint tables (via the detectors) and WRITES append-only findings rows
// (+ sets a metrics gauge). It NEVER resolves or adjudicates — the never-auto-act invariant.
package poolroyalty

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/metrics"
)

// Finding is one flagged detector result. Metrics holds the full flag struct — the
// FindingsWriter marshals it to the JSONB evidence column.
type Finding struct {
	Economy              string
	Detector             string
	IdentityKey          string
	ContributorWorkspace string
	RequesterWorkspace   string // "" where the detector has no single requester (similarity)
	EntryOrContent       string // entry_id (cache) / content_hash (distill); "" for bilateral
	WindowSeconds        int64
	Metrics              any
}

// findingsRecorder is the ONLY write surface the sweep holds: Record inserts one finding
// (append-only — the impl is INSERT … ON CONFLICT (identity_key) DO NOTHING). It exposes
// NO generic Exec, NO ledger, NO revoke.
type findingsRecorder interface {
	Record(ctx context.Context, f Finding) (inserted bool, err error)
}

// sweepDetector / sweepDistillDetector are the read-only detector seams — satisfied by the
// same *DetectorReader / *DistillDetectorReader the /detect endpoints use.
type sweepDetector interface {
	VolumeConcentration(context.Context, time.Duration) ([]VolumeFlag, error)
	BilateralConcentration(context.Context, time.Duration) ([]SelfDealingFlag, error)
	SimilarityGaming(context.Context, time.Duration) ([]SimilarityFlag, error)
}

type sweepDistillDetector interface {
	VolumeConcentration(context.Context, time.Duration) ([]DistillVolumeFlag, error)
	BilateralConcentration(context.Context, time.Duration) ([]DistillSelfDealingFlag, error)
}

// DetectorSweep runs the detectors on a cadence and records flagged findings. The zero/nil
// sweep is inert. It can read mints + write findings + set a gauge — and NOTHING else.
type DetectorSweep struct {
	cache   sweepDetector
	distill sweepDistillDetector
	sink    findingsRecorder
	window  time.Duration
}

// NewDetectorSweep wires the read-only detectors + the append-only findings sink + the
// rolling detection window. A nil cache/distill simply skips that economy.
func NewDetectorSweep(cache sweepDetector, distill sweepDistillDetector, sink findingsRecorder, window time.Duration) *DetectorSweep {
	if window <= 0 {
		window = 24 * time.Hour
	}
	return &DetectorSweep{cache: cache, distill: distill, sink: sink, window: window}
}

func idKey(parts ...string) string { return SHA256Hex([]byte(strings.Join(parts, ":"))) }

// RunOnce runs every detector once, records the FLAGGED findings (append-only, deduped by
// identity_key), and sets the flagged-count gauge per (economy, detector) — including 0 (so
// the gauge reflects the current picture). It READS the mint tables and WRITES only findings;
// it never touches a mint/balance/held row. A per-detector or per-record error is noted and
// the sweep continues (RunOnce returns the first error; StartScheduler logs it).
func (s *DetectorSweep) RunOnce(ctx context.Context) error {
	if s == nil || s.sink == nil {
		return nil
	}
	winSecs := int64(s.window / time.Second)
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	rec := func(f Finding) {
		if _, err := s.sink.Record(ctx, f); err != nil {
			note(err)
		}
	}

	if s.cache != nil {
		if flags, err := s.cache.VolumeConcentration(ctx, s.window); err != nil {
			note(err)
		} else {
			n := 0
			for _, f := range flags {
				if !f.Flagged {
					continue
				}
				n++
				rec(Finding{Economy: "cache", Detector: "volume",
					IdentityKey:          idKey("cache", "volume", f.EntryID, f.ContributorWorkspace, f.RequesterWorkspace),
					ContributorWorkspace: f.ContributorWorkspace, RequesterWorkspace: f.RequesterWorkspace,
					EntryOrContent: f.EntryID, WindowSeconds: winSecs, Metrics: f})
			}
			metrics.SetRoyaltyDetectorFlagged("cache", "volume", float64(n))
		}
		if flags, err := s.cache.BilateralConcentration(ctx, s.window); err != nil {
			note(err)
		} else {
			n := 0
			for _, f := range flags {
				if !f.Flagged {
					continue
				}
				n++
				rec(Finding{Economy: "cache", Detector: "bilateral",
					IdentityKey:          idKey("cache", "bilateral", f.ContributorWorkspace, f.RequesterWorkspace),
					ContributorWorkspace: f.ContributorWorkspace, RequesterWorkspace: f.RequesterWorkspace,
					WindowSeconds: winSecs, Metrics: f})
			}
			metrics.SetRoyaltyDetectorFlagged("cache", "bilateral", float64(n))
		}
		if flags, err := s.cache.SimilarityGaming(ctx, s.window); err != nil {
			note(err)
		} else {
			n := 0
			for _, f := range flags {
				if !f.Flagged {
					continue
				}
				n++
				rec(Finding{Economy: "cache", Detector: "similarity",
					IdentityKey:          idKey("cache", "similarity", f.ContributorWorkspace, f.EntryID),
					ContributorWorkspace: f.ContributorWorkspace, EntryOrContent: f.EntryID,
					WindowSeconds: winSecs, Metrics: f})
			}
			metrics.SetRoyaltyDetectorFlagged("cache", "similarity", float64(n))
		}
	}

	if s.distill != nil {
		if flags, err := s.distill.VolumeConcentration(ctx, s.window); err != nil {
			note(err)
		} else {
			n := 0
			for _, f := range flags {
				if !f.Flagged {
					continue
				}
				n++
				rec(Finding{Economy: "distill", Detector: "volume",
					IdentityKey:          idKey("distill", "volume", f.ContentHash, f.ContributorWorkspace, f.RequesterWorkspace),
					ContributorWorkspace: f.ContributorWorkspace, RequesterWorkspace: f.RequesterWorkspace,
					EntryOrContent: f.ContentHash, WindowSeconds: winSecs, Metrics: f})
			}
			metrics.SetRoyaltyDetectorFlagged("distill", "volume", float64(n))
		}
		if flags, err := s.distill.BilateralConcentration(ctx, s.window); err != nil {
			note(err)
		} else {
			n := 0
			for _, f := range flags {
				if !f.Flagged {
					continue
				}
				n++
				rec(Finding{Economy: "distill", Detector: "bilateral",
					IdentityKey:          idKey("distill", "bilateral", f.ContributorWorkspace, f.RequesterWorkspace),
					ContributorWorkspace: f.ContributorWorkspace, RequesterWorkspace: f.RequesterWorkspace,
					WindowSeconds: winSecs, Metrics: f})
			}
			metrics.SetRoyaltyDetectorFlagged("distill", "bilateral", float64(n))
		}
	}
	return firstErr
}

// StartScheduler ticks RunOnce until ctx ends — mirrors FinalizeSweeper.StartScheduler.
func (s *DetectorSweep) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Hour
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RunOnce(ctx); err != nil {
				slog.Warn("poolroyalty: detector sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}
