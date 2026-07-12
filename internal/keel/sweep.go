package keel

import (
	"context"
	"log/slog"
	"time"
)

// Sweep is the read-only cross-tenant drift detector run on a cadence and recorded append-only. It mirrors
// poolroyalty.DetectorSweep: it holds a QUERY-ONLY Reader + an append-only FindingsWriter and NOTHING that
// can mint/burn/act. The whole sweep is gated OFF by default at the wiring site (LENS_KEEL_ENABLED) and
// runs leader-elected (registered via leader.Run in main.go), NOT a plain ticker.
type Sweep struct {
	reader        *Reader
	writer        *FindingsWriter
	cfg           Config
	windowSeconds int64
	lookback      time.Duration
	now           func() time.Time
	log           *slog.Logger
}

// NewSweep wires the read source + append-only sink + thresholds. lookback bounds how far back the corpus
// is read (enough to cover baseline + current windows).
func NewSweep(reader *Reader, writer *FindingsWriter, cfg Config, windowSeconds int64, lookback time.Duration) *Sweep {
	if cfg.MinWorkspaces <= 0 {
		cfg.MinWorkspaces = DefaultMinWorkspaces
	}
	if windowSeconds <= 0 {
		windowSeconds = 3600
	}
	if lookback <= 0 {
		lookback = 48 * time.Hour
	}
	return &Sweep{
		reader: reader, writer: writer, cfg: cfg,
		windowSeconds: windowSeconds, lookback: lookback,
		now: time.Now, log: slog.Default(),
	}
}

// RunOnce reads the consented corpus, computes drift findings, and records each append-only. Returns the
// count newly inserted (a re-sweep of the same window inserts 0 — ON CONFLICT dedup). Read-only over the
// corpus; the only write is keel_findings.
func (s *Sweep) RunOnce(ctx context.Context) (int, error) {
	since := s.now().Add(-s.lookback)
	obs, err := s.reader.CohortObservations(ctx, s.windowSeconds, since)
	if err != nil {
		return 0, err
	}
	findings := Detect(obs, s.cfg)
	recorded := 0
	for _, f := range findings {
		metrics := map[string]any{
			"cohort_mean":    f.CohortMean,
			"cohort_stddev":  f.CohortStdDev,
			"residual_shift": f.ResidualShift,
			"window_seconds": s.windowSeconds,
		}
		ins, err := s.writer.Record(ctx, f, metrics)
		if err != nil {
			s.log.Warn("keel: record finding failed",
				slog.String("workspace_id", f.WorkspaceID), slog.String("unit", f.Unit),
				slog.String("err", err.Error()))
			continue
		}
		if ins {
			recorded++
		}
	}
	return recorded, nil
}

// StartScheduler ticks RunOnce until ctx is cancelled. In production this runs INSIDE a leader.Run callback
// (main.go), so exactly one instance sweeps.
func (s *Sweep) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Hour
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.RunOnce(ctx); err != nil {
				s.log.Error("keel: sweep failed", slog.String("err", err.Error()))
			} else if n > 0 {
				s.log.Info("keel: drift findings recorded", slog.Int("count", n))
			}
		}
	}
}
