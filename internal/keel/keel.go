// Package keel is cross-tenant DRIFT ATTRIBUTION (unit U25) — the population axis alongside U21's
// per-tenant-over-time temporal path (internal/anomaly). It reads ONLY the already-consented routing
// corpus (routing_patterns.output_quality WHERE opted_in = TRUE), aggregates a quality cohort per
// comparison unit + time window with a ≥ MinWorkspaces distinct-workspace floor (dual opt-out reused
// verbatim from the routing Advisor), and — for each opted-in workspace — reports how far it deviates
// from its cohort AND whether that deviation is IDIOSYNCRATIC (this tenant drifted relative to its peers)
// or COMMON-MODE (the whole cohort drifted together — never attributed to a single tenant).
//
// TENANCY BOUNDARY (the whole point): a finding names ONLY the SELF workspace + a deviation sigma. Every
// cross-tenant value enters strictly as an AGGREGATE (cohort mean/stddev over ≥ MinWorkspaces distinct
// workspaces); no other tenant's id or raw output_quality is ever emitted or recoverable. Below the floor,
// nothing is emitted. Keel is READ-ONLY over the corpus (it only writes its OWN append-only findings) and
// structurally NEVER acts on money/ledger/held tables — the detector-sweep discipline (poolroyalty).
//
// The sigma math MIRRORS internal/anomaly.computeStats EXACTLY (population stddev, sqrt(sqDiff/n)) — it is
// re-implemented rather than called because that func is unexported and internal/anomaly must stay
// untouched. Parity is pinned by TestComputeStatsParity.
package keel

import (
	"math"
	"sort"
)

// AttributionIdiosyncratic / AttributionCommonMode are the two attribution labels.
const (
	AttributionIdiosyncratic = "idiosyncratic"
	AttributionCommonMode    = "common_mode"
)

// DefaultMinWorkspaces mirrors routing's defaultMinWorkspaces = 3 (the privacy floor). NOT re-calibrated.
const DefaultMinWorkspaces = 3

// Config is the (default-OFF-gated) threshold seam. Placeholder defaults — CALIBRATE AT N3 TURN-ON;
// synthetic data only proves the mechanism, never the thresholds.
type Config struct {
	MinWorkspaces  int     // ≥ distinct opted-in workspaces per cohort or the cohort is withheld (default 3)
	DeviationSigma float64 // |deviation| ≥ this (in cohort-stddev units) to emit a finding (placeholder)
}

// DefaultConfig returns the placeholder thresholds. The sweep is separately behind a default-off flag.
func DefaultConfig() Config {
	return Config{MinWorkspaces: DefaultMinWorkspaces, DeviationSigma: 3.0}
}

// Observation is one per-(unit, workspace, window) aggregated quality point from the CONSENTED corpus.
// MeanQuality is already the AVG(output_quality) for that workspace's rows in that (unit, window) — a
// cross-tenant value never reaches the detector un-aggregated.
type Observation struct {
	Unit        string  // the comparison unit (provider|model) — see the chosen dimension in the report
	WorkspaceID string  // the OWN workspace this point belongs to
	Window      int64   // integer window bucket (older < newer)
	MeanQuality float64 // AVG(output_quality) for (unit, workspace, window)
	Sample      int     // COUNT(*) rows behind the mean (informational)
}

// Finding is the ONLY thing Keel emits. It carries the self workspace, the unit, the current window, the
// deviation sigma, the attribution label, and the cohort size — NEVER another tenant's id or raw value.
type Finding struct {
	WorkspaceID    string  `json:"workspace_id"`
	Unit           string  `json:"unit"`
	Window         int64   `json:"window"`
	DeviationSigma float64 `json:"deviation_sigma"`
	Attribution    string  `json:"attribution"`
	CohortN        int     `json:"cohort_n"` // distinct opted-in workspaces in the current window's cohort (≥ MinWorkspaces)
	// Evidence — AGGREGATES ONLY (cohort-level, never a counterparty's raw value). Rides the metrics
	// JSONB, not separate emitted columns. Safe to expose: a cohort mean/stddev over ≥ MinWorkspaces
	// distinct workspaces cannot be inverted to any single tenant's value.
	CohortMean    float64 `json:"cohort_mean"`
	CohortStdDev  float64 `json:"cohort_stddev"`
	ResidualShift float64 `json:"residual_shift"`
}

// stats is the moment pair Detect needs; mirrors anomaly.WindowStats{Mean,StdDev} with the SAME formula.
type stats struct {
	mean   float64
	stdDev float64
	n      int
}

// computeStats mirrors internal/anomaly.computeStats byte-for-byte: population stddev = sqrt(sqDiff/n).
// Pinned to the original by TestComputeStatsParity.
func computeStats(values []float64) *stats {
	n := len(values)
	if n == 0 {
		return nil
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(n)
	var sqDiff float64
	for _, v := range values {
		d := v - mean
		sqDiff += d * d
	}
	return &stats{mean: mean, stdDev: math.Sqrt(sqDiff / float64(n)), n: n}
}

// Detect is the pure, DETERMINISTIC core. Given per-(unit, workspace, window) consented means, for each
// unit it takes the two most-recent windows (baseline, current), enforces the ≥ MinWorkspaces floor on
// BOTH (withhold otherwise), and for every workspace present in both computes:
//
//	r_cur  = ws_mean_cur  − cohortMean_cur         (this tenant's standing vs its cohort NOW)
//	r_base = ws_mean_base − cohortMean_base        (its standing in the prior window)
//	deviation_sigma = r_cur / cohortStdDev_cur
//	residual_shift  = r_cur − r_base               (how much its RELATIVE position moved)
//
// A finding is emitted when |deviation_sigma| ≥ cfg.DeviationSigma. It is IDIOSYNCRATIC when the tenant's
// residual MOVED (|residual_shift| ≥ cfg.DeviationSigma × cohortStdDev_cur) — it drifted relative to peers;
// otherwise COMMON-MODE — it is an outlier but its relative position is stable, so any cohort-wide shift is
// common to all and never attributed to this single tenant. Output is sorted (unit, workspace) for
// byte-identical determinism.
func Detect(obs []Observation, cfg Config) []Finding {
	if cfg.MinWorkspaces <= 0 {
		cfg.MinWorkspaces = DefaultMinWorkspaces
	}
	// Group by unit → window → workspace → mean (deterministic maps drained via sorted keys).
	byUnit := map[string]map[int64]map[string]float64{}
	for _, o := range obs {
		if byUnit[o.Unit] == nil {
			byUnit[o.Unit] = map[int64]map[string]float64{}
		}
		if byUnit[o.Unit][o.Window] == nil {
			byUnit[o.Unit][o.Window] = map[string]float64{}
		}
		byUnit[o.Unit][o.Window][o.WorkspaceID] = o.MeanQuality
	}

	var out []Finding
	units := sortedKeys(byUnit)
	for _, unit := range units {
		windows := byUnit[unit]
		wins := sortedInt64Keys(windows)
		if len(wins) < 2 {
			continue // need a baseline + a current window to separate common-mode from idiosyncratic
		}
		base := windows[wins[len(wins)-2]]
		cur := windows[wins[len(wins)-1]]
		curWin := wins[len(wins)-1]

		// Floor on BOTH windows: a below-floor cohort is WITHHELD entirely (no output, single value unrecoverable).
		if len(cur) < cfg.MinWorkspaces || len(base) < cfg.MinWorkspaces {
			continue
		}
		curMeanStats := computeStats(mapValsSorted(cur))
		baseStats := computeStats(mapValsSorted(base))
		if curMeanStats == nil || baseStats == nil || curMeanStats.stdDev == 0 {
			continue // a zero-variance cohort yields no meaningful sigma
		}

		for _, ws := range sortedKeys(cur) {
			baseQ, ok := base[ws]
			if !ok {
				continue // only workspaces present in BOTH windows can be common-mode-vs-idiosyncratic separated
			}
			rCur := cur[ws] - curMeanStats.mean
			rBase := baseQ - baseStats.mean
			devSigma := rCur / curMeanStats.stdDev
			residualShift := rCur - rBase

			if math.Abs(devSigma) < cfg.DeviationSigma {
				continue
			}
			attribution := AttributionCommonMode
			if math.Abs(residualShift) >= cfg.DeviationSigma*curMeanStats.stdDev {
				attribution = AttributionIdiosyncratic
			}
			out = append(out, Finding{
				WorkspaceID:    ws,
				Unit:           unit,
				Window:         curWin,
				DeviationSigma: devSigma,
				Attribution:    attribution,
				CohortN:        len(cur),
				CohortMean:     curMeanStats.mean,
				CohortStdDev:   curMeanStats.stdDev,
				ResidualShift:  residualShift,
			})
		}
	}
	// Deterministic order: (unit, workspace).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Unit != out[j].Unit {
			return out[i].Unit < out[j].Unit
		}
		return out[i].WorkspaceID < out[j].WorkspaceID
	})
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedInt64Keys[V any](m map[int64]V) []int64 {
	ks := make([]int64, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}

// mapValsSorted returns the map's values ordered by KEY, so the reduction into computeStats is
// order-stable (byte-identical sigma across runs).
func mapValsSorted(m map[string]float64) []float64 {
	ks := sortedKeys(m)
	vs := make([]float64, len(ks))
	for i, k := range ks {
		vs[i] = m[k]
	}
	return vs
}
