package keel

import (
	"math"
	"sort"
)

// HARDENED (money-grade) detection mode — U25 K3. ADDITIVE + DEFAULT-OFF: the existing Detect, its
// behaviour, and every ordinary finding are UNCHANGED. This path exists so Keel findings can LATER gate
// money (H5 provenance bonds); it is deliberately more conservative and gaming-resistant than Detect:
//
//   - LEAVE-ONE-OUT: each workspace is scored against the cohort of OTHERS (excluded from the median/scale
//     it is judged against), closing the self-drag where a workspace's own value pulls the baseline toward
//     itself (Detect includes the judged ws in its mean — keel.go:149/160).
//   - MEDIAN + MAD (scaled 1.4826) instead of mean + population stddev: robust to < 50% cohort
//     contamination, so sock workspaces cannot move the baseline. MAD==0 (degenerate cohort) WITHHOLDS.
//   - MONEY-GRADE FLOORS: the leave-one-out cohort must have >= MoneyCohortFloor (>> the privacy floor of
//     3) distinct OTHER workspaces, and the judged workspace must have >= MinSamples requests in the
//     window — below either, WITHHOLD.
//   - PERSISTENCE: the drop must hold for >= PersistenceWindows consecutive most-recent windows.
//   - DIRECTION: only a quality DROP emits (a workspace robustly BELOW its cohort), never an upward spike.
//   - Attribution is ALWAYS idiosyncratic by construction: a common-mode (cohort-wide) shift moves the
//     OTHERS too, so a leave-one-out drop cannot arise from it — common_mode is never emitted here.
//
// NO MONEY MAY MOVE ON THESE THRESHOLDS UNTIL N3 CALIBRATION — the config values are placeholders.

// madScaleFactor makes MAD a consistent estimator of the stddev for a normal distribution, so the robust
// score stays interpretable in sigma-like units.
const madScaleFactor = 1.4826

// HardenedMode / OrdinaryMode label a finding's provenance so hardened findings are distinguishable from
// ordinary ones at the SQL layer (keel_findings.mode). H5 may read ONLY mode = HardenedMode.
const (
	HardenedMode = "hardened"
	OrdinaryMode = "ordinary"
)

// HardenedConfig — the money-grade knobs. ALL PLACEHOLDERS — calibrate at N3 against real-scale
// distribution; NO MONEY MAY MOVE ON AN UNCALIBRATED THRESHOLD.
type HardenedConfig struct {
	MoneyCohortFloor   int     // >= this many distinct OTHER workspaces (leave-one-out) or WITHHOLD (>> 3)
	MinSamples         int     // judged ws needs >= this many requests in a window or WITHHOLD
	PersistenceWindows int     // the drop must hold for >= this many consecutive most-recent windows
	HardenedSigma      float64 // |robust score| >= this (MAD-scaled units) to emit; a DROP (below) only
}

// DefaultHardenedConfig returns the placeholder money-grade thresholds.
// PLACEHOLDER — calibrate at N3 against real-scale distribution; NO MONEY MAY MOVE ON AN UNCALIBRATED
// THRESHOLD. Synthetic data only proves the mechanism, never these values.
func DefaultHardenedConfig() HardenedConfig {
	return HardenedConfig{
		MoneyCohortFloor:   10,  // PLACEHOLDER — calibrate at N3; NO MONEY MAY MOVE ON AN UNCALIBRATED THRESHOLD
		MinSamples:         30,  // PLACEHOLDER — calibrate at N3; NO MONEY MAY MOVE ON AN UNCALIBRATED THRESHOLD
		PersistenceWindows: 3,   // PLACEHOLDER — calibrate at N3; NO MONEY MAY MOVE ON AN UNCALIBRATED THRESHOLD
		HardenedSigma:      3.5, // PLACEHOLDER — calibrate at N3; NO MONEY MAY MOVE ON AN UNCALIBRATED THRESHOLD
	}
}

// median returns the median of a COPY of vals (never mutates the caller's slice). len(vals) must be > 0.
func median(vals []float64) float64 {
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// madScaled returns 1.4826 * median(|v - med|) — the robust scale. Returns 0 for a degenerate cohort
// (the middle absolute deviation is 0), which callers MUST treat as WITHHOLD (never divide by it).
func madScaled(vals []float64, med float64) float64 {
	dev := make([]float64, len(vals))
	for i, v := range vals {
		dev[i] = math.Abs(v - med)
	}
	return madScaleFactor * median(dev)
}

// obsPoint is one workspace's aggregate in one window: the AVG quality and the COUNT behind it.
type obsPoint struct {
	mean   float64
	sample int
}

// hWindow is the leave-one-out robust evaluation of one (ws, window).
type hWindow struct {
	others int     // distinct OTHER workspaces in the leave-one-out cohort (>= MoneyCohortFloor)
	median float64 // median of the OTHERS' means
	mad    float64 // 1.4826 * MAD of the OTHERS' means (> 0)
	score  float64 // (ws_mean - othersMedian) / othersMad ; negative = BELOW cohort (a drop)
}

// leaveOneOut scores ws against the cohort of OTHERS in `cohort`. ok=false ⇒ WITHHOLD this window
// (ws absent, ws under MinSamples, fewer than MoneyCohortFloor others, or a degenerate MAD==0 cohort).
func leaveOneOut(cohort map[string]obsPoint, ws string, hcfg HardenedConfig) (hWindow, bool) {
	self, ok := cohort[ws]
	if !ok || self.sample < hcfg.MinSamples {
		return hWindow{}, false
	}
	others := make([]float64, 0, len(cohort))
	for k, p := range cohort {
		if k == ws {
			continue // LEAVE-ONE-OUT: the judged ws never enters the baseline it is judged against.
		}
		others = append(others, p.mean)
	}
	if len(others) < hcfg.MoneyCohortFloor {
		return hWindow{}, false
	}
	med := median(others)
	mad := madScaled(others, med)
	if mad == 0 {
		return hWindow{}, false // degenerate cohort — WITHHOLD (no divide-by-zero, no infinite score).
	}
	return hWindow{others: len(others), median: med, mad: mad, score: (self.mean - med) / mad}, true
}

// DetectHardened is the pure, DETERMINISTIC money-grade core. For each unit it takes the most-recent
// PersistenceWindows windows and emits a Finding for a workspace ONLY when, in EVERY one of those windows,
// the workspace is robustly BELOW its leave-one-out cohort by >= HardenedSigma (median/MAD, drop-only,
// floors enforced). Output is sorted (unit, workspace) for byte-identical determinism. See the file header.
func DetectHardened(obs []Observation, hcfg HardenedConfig) []Finding {
	if hcfg.MoneyCohortFloor < DefaultMinWorkspaces {
		hcfg.MoneyCohortFloor = DefaultMinWorkspaces // never let the leave-one-out cohort fall below the privacy floor
	}
	if hcfg.PersistenceWindows < 1 {
		hcfg.PersistenceWindows = 1
	}
	byUnit := map[string]map[int64]map[string]obsPoint{}
	for _, o := range obs {
		if byUnit[o.Unit] == nil {
			byUnit[o.Unit] = map[int64]map[string]obsPoint{}
		}
		if byUnit[o.Unit][o.Window] == nil {
			byUnit[o.Unit][o.Window] = map[string]obsPoint{}
		}
		byUnit[o.Unit][o.Window][o.WorkspaceID] = obsPoint{mean: o.MeanQuality, sample: o.Sample}
	}

	var out []Finding
	for _, unit := range sortedKeys(byUnit) {
		windows := byUnit[unit]
		wins := sortedInt64Keys(windows)
		if len(wins) < hcfg.PersistenceWindows {
			continue // not enough history to satisfy persistence
		}
		lastK := wins[len(wins)-hcfg.PersistenceWindows:]
		curWin := wins[len(wins)-1]

		for _, ws := range sortedKeys(windows[curWin]) {
			var cur hWindow
			persistent := true
			for i, w := range lastK {
				hw, ok := leaveOneOut(windows[w], ws, hcfg)
				// Must be a DROP of at least HardenedSigma in EVERY window: score <= -HardenedSigma.
				// score > -HardenedSigma covers both "not low enough" and an upward spike (direction).
				if !ok || hw.score > -hcfg.HardenedSigma {
					persistent = false
					break
				}
				if i == len(lastK)-1 {
					cur = hw // the current window's stats ride the finding
				}
			}
			if !persistent {
				continue
			}
			out = append(out, Finding{
				WorkspaceID:    ws,
				Unit:           unit,
				Window:         curWin,
				DeviationSigma: cur.score,                // negative robust score (a drop), MAD-scaled units
				Attribution:    AttributionIdiosyncratic, // by construction — leave-one-out excludes common-mode
				CohortN:        cur.others,               // leave-one-out cohort size (>= MoneyCohortFloor >> 3)
				CohortMean:     cur.median,               // robust: median of the OTHERS (never a raw counterparty value)
				CohortStdDev:   cur.mad,                  // robust: MAD-scaled of the OTHERS
				ResidualShift:  cur.score,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Unit != out[j].Unit {
			return out[i].Unit < out[j].Unit
		}
		return out[i].WorkspaceID < out[j].WorkspaceID
	})
	return out
}
