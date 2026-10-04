package routing

// B27.23 — routing to a cheaper model never trades quality.
//
// The router decides a downgrade from the prompt's shape (substring scans for
// "analyse", code fences, length). That guess says nothing about whether the
// cheaper model actually answers this kind of request as well. The QualityGate
// is the measured check every cost downgrade must pass before it stands:
//
//   - A cohort is the request's (feature, input-size range) — the same
//     privacy-bucketed key routing_patterns stores and the Advisor ranks.
//   - The cohort's FLOOR is the quality the model being replaced measured on
//     that cohort (AVG(output_quality) over opted-in, live-captured rows).
//   - The downgrade stands only when the cheaper model's measured quality on
//     the same cohort is AT OR ABOVE that floor.
//   - Either side unmeasured — fewer than MinSamples rows, or fewer than
//     MinWorkspaces contributing workspaces (the Advisor's privacy floor) —
//     holds the downgrade: no measurement, no trade.
//
// Like the Advisor, the per-request call is an in-memory lookup; the corpus is
// reloaded on a timer. Unlike the Advisor it is not behind an enable flag: it
// can only ever KEEP the more capable model, never pick one.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/mining"
)

// measured is one model's quality on one cohort.
type measured struct {
	quality    float64
	samples    int
	workspaces int
}

// GateVerdict is the gate's answer for one proposed downgrade.
type GateVerdict struct {
	Allowed bool
	Floor   float64 // the replaced model's measured quality on the cohort (0 when unmeasured)
	Quality float64 // the cheaper model's measured quality on the cohort (0 when unmeasured)
	Reason  string
}

// QualityGate holds the measured per-cohort, per-model quality.
type QualityGate struct {
	src cohortSource
	cfg Config

	mu          sync.RWMutex
	cohorts     map[string]map[string]measured // feature|input_range → provider|model → measured
	lastRefresh time.Time
}

// NewQualityGate builds the gate over the pattern aggregate (production:
// *mining.PatternMiner). Only MinSamples, MinWorkspaces and RefreshInterval are
// read from cfg; zero values take the Advisor's defaults.
func NewQualityGate(src cohortSource, cfg Config) *QualityGate {
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = defaultMinSamples
	}
	if cfg.MinWorkspaces <= 0 {
		cfg.MinWorkspaces = defaultMinWorkspaces
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = defaultRefreshInterval
	}
	return &QualityGate{src: src, cfg: cfg, cohorts: map[string]map[string]measured{}}
}

func modelKey(provider, model string) string { return provider + "|" + model }

// Refresh reloads the measured qualities from routing_patterns.
func (g *QualityGate) Refresh(ctx context.Context) error {
	if g == nil || g.src == nil {
		return nil
	}
	stats, err := g.src.AggregateCohorts(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]map[string]measured)
	for _, s := range stats {
		k := cohortKey(s.FeatureCategory, s.InputTokenRange)
		if next[k] == nil {
			next[k] = map[string]measured{}
		}
		next[k][modelKey(s.ProviderUsed, s.ModelUsed)] = measured{quality: s.AvgQuality, samples: s.SampleCount, workspaces: s.DistinctWorkspaces}
	}
	g.mu.Lock()
	g.cohorts = next
	g.lastRefresh = time.Now()
	g.mu.Unlock()
	return nil
}

// StartRefresh refreshes now, then on the configured interval until ctx ends.
func (g *QualityGate) StartRefresh(ctx context.Context) {
	if g == nil || g.src == nil {
		return
	}
	if err := g.Refresh(ctx); err != nil {
		slog.Warn("routing quality gate: initial refresh failed", slog.String("err", err.Error()))
	}
	go func() {
		t := time.NewTicker(g.cfg.RefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := g.Refresh(ctx); err != nil {
					slog.Warn("routing quality gate: refresh failed", slog.String("err", err.Error()))
				}
			}
		}
	}()
}

// Downgrade answers whether serving `to` in place of `from` keeps quality for a
// request of this cohort. The caller has already established that `to` is the
// cheaper model; the gate only judges quality.
func (g *QualityGate) Downgrade(feature string, inputTokens int, provider, from, to string) GateVerdict {
	inputRange := mining.InputBucketFor(inputTokens)
	cohort := feature + "/" + inputRange
	if g == nil {
		return GateVerdict{Reason: "no quality measurements loaded"}
	}
	g.mu.RLock()
	c := g.cohorts[cohortKey(feature, inputRange)]
	floor, okF := c[modelKey(provider, from)]
	cheap, okT := c[modelKey(provider, to)]
	g.mu.RUnlock()

	thin := func(m measured, ok bool) bool {
		return !ok || m.samples < g.cfg.MinSamples || m.workspaces < g.cfg.MinWorkspaces
	}
	need := fmt.Sprintf("needs %d samples across %d workspaces", g.cfg.MinSamples, g.cfg.MinWorkspaces)
	switch {
	case thin(floor, okF):
		return GateVerdict{Reason: fmt.Sprintf("kept %s: its quality on %s is not measured yet (%s)", from, cohort, need)}
	case thin(cheap, okT):
		return GateVerdict{Floor: floor.quality, Reason: fmt.Sprintf("kept %s: %s's quality on %s is not measured yet (%s)", from, to, cohort, need)}
	case cheap.quality < floor.quality:
		return GateVerdict{Floor: floor.quality, Quality: cheap.quality,
			Reason: fmt.Sprintf("kept %s: %s measures %.3f on %s, below the %.3f floor %s sets", from, to, cheap.quality, cohort, floor.quality, from)}
	}
	return GateVerdict{Allowed: true, Floor: floor.quality, Quality: cheap.quality,
		Reason: fmt.Sprintf("%s measures %.3f on %s, at or above the %.3f floor %s sets (%d samples across %d workspaces)",
			to, cheap.quality, cohort, floor.quality, from, cheap.samples, cheap.workspaces)}
}
