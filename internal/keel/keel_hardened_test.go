package keel_test

import (
	"reflect"
	"testing"

	"github.com/talyvor/lens/internal/keel"
)

// hobs builds one hardened Observation (with a sample count, which DetectHardened enforces via MinSamples).
func hobs(unit, ws string, win int64, q float64, sample int) keel.Observation {
	return keel.Observation{Unit: unit, WorkspaceID: ws, Window: win, MeanQuality: q, Sample: sample}
}

func flagged(fs []keel.Finding, ws string) *keel.Finding {
	for i := range fs {
		if fs[i].WorkspaceID == ws {
			return &fs[i]
		}
	}
	return nil
}

const u = "openai/gpt-4o"

// median + MAD helper correctness (pins the robust-stat primitives).
func TestHardened_MedianMADHelpers(t *testing.T) {
	// median is exercised indirectly; assert the documented MAD scale via a known case through DetectHardened
	// is covered elsewhere. Here pin median parity on an even + odd set via the exported behaviour: a cohort
	// whose others are [1,2,3,4] has median 2.5. We reach it through a withhold-free drop.
	obs := []keel.Observation{
		hobs(u, "A", 1, -100, 5), // far below → guaranteed drop regardless of exact scale
		hobs(u, "o1", 1, 1, 5), hobs(u, "o2", 1, 2, 5), hobs(u, "o3", 1, 3, 5), hobs(u, "o4", 1, 4, 5),
	}
	f := keel.DetectHardened(obs, keel.HardenedConfig{MoneyCohortFloor: 4, MinSamples: 1, PersistenceWindows: 1, HardenedSigma: 1.0})
	a := flagged(f, "A")
	if a == nil {
		t.Fatal("A (far below) must be flagged")
	}
	// others = [1,2,3,4] → median 2.5, MAD-scaled = 1.4826*median(1.5,0.5,0.5,1.5)=1.4826*1.0=1.4826
	if got, want := a.CohortMean, 2.5; got != want {
		t.Errorf("others median: got %v want %v", got, want)
	}
	if got, want := a.CohortStdDev, 1.4826; got != want {
		t.Errorf("others MAD-scaled: got %v want %v", got, want)
	}
}

// THE CRUX #1 — leave-one-out defeats SELF-DRAG. A genuinely-low workspace escapes the self-included mean
// (its own low value drags the baseline toward it); leave-one-out judges it against the honest peers only.
func TestHardened_LeaveOneOut_DefeatsSelfDrag(t *testing.T) {
	var obs []keel.Observation
	for _, w := range []int64{1, 2} { // two identical windows
		obs = append(obs,
			hobs(u, "A", w, 0.30, 100), // genuinely low — SHOULD be caught
			hobs(u, "h1", w, 0.82, 100),
			hobs(u, "h2", w, 0.85, 100),
			hobs(u, "h3", w, 0.88, 100),
		)
	}
	// OLD path (Detect: mean, self-INCLUDED, σ=2.0): A's 0.30 drags the cohort mean to ~0.71, so its
	// deviation shrinks to ~-1.73 and A ESCAPES. (Prove the vulnerability, don't assert it.)
	old := keel.Detect(obs, keel.Config{MinWorkspaces: 3, DeviationSigma: 2.0})
	if f := flagged(old, "A"); f != nil {
		t.Fatalf("self-drag premise broken: Detect flagged A (devSigma=%.3f); expected ESCAPE under the self-included mean", f.DeviationSigma)
	}
	// NEW path (DetectHardened: leave-one-out median/MAD): A judged against {0.82,0.85,0.88} only → CAUGHT.
	got := keel.DetectHardened(obs, keel.HardenedConfig{MoneyCohortFloor: 3, MinSamples: 1, PersistenceWindows: 1, HardenedSigma: 3.0})
	a := flagged(got, "A")
	if a == nil {
		t.Fatalf("leave-one-out must CATCH the self-dragging workspace A; got %+v", got)
	}
	if a.Attribution != keel.AttributionIdiosyncratic || a.DeviationSigma > -3.0 {
		t.Errorf("A must be idiosyncratic, score <= -3.0; got attribution=%s score=%.3f", a.Attribution, a.DeviationSigma)
	}
	if len(got) != 1 {
		t.Errorf("only A should be flagged; got %d: %+v", len(got), got)
	}
}

// THE CRUX #2 — median/MAD defeats SOCK CONTAMINATION. A guilty workspace floods same-level sock
// workspaces to drag the MEAN toward itself (escaping Detect even harder); the median is robust to the
// < 50% socks, so DetectHardened still catches A. (Note: minority socks cannot FRAME an honest rival under
// either method — an honest point's mean-deviation is bounded by sqrt(s/(n-s)), which needs > 80% socks to
// cross 2σ, at which point the median is no longer robust either. The real, demonstrable mean-vulnerability
// is this ESCAPE, which robust stats defeat.)
func TestHardened_MedianMAD_DefeatsSockContamination(t *testing.T) {
	var obs []keel.Observation
	for _, w := range []int64{1, 2} {
		obs = append(obs, hobs(u, "A", w, 0.30, 100)) // guilty
		for _, s := range []string{"s1", "s2", "s3", "s4", "s5"} {
			obs = append(obs, hobs(u, s, w, 0.30, 100)) // 5 same-level socks (45% of a 12-cohort)
		}
		for i, q := range []float64{0.82, 0.83, 0.85, 0.85, 0.87, 0.88} { // 6 honest
			obs = append(obs, hobs(u, string(rune('a'+i)), w, q, 100))
		}
	}
	// OLD: 6 low (A+socks) drag the mean to ~0.575 → A devSigma ~-1.0 → ESCAPES.
	if f := flagged(keel.Detect(obs, keel.Config{MinWorkspaces: 3, DeviationSigma: 2.0}), "A"); f != nil {
		t.Fatalf("sock premise broken: Detect flagged A (devSigma=%.3f); expected sock-drag ESCAPE", f.DeviationSigma)
	}
	// NEW: median of A's 11 others stays ~0.82 (socks are a minority) → A score ~-5.8 → CAUGHT.
	got := keel.DetectHardened(obs, keel.HardenedConfig{MoneyCohortFloor: 6, MinSamples: 1, PersistenceWindows: 1, HardenedSigma: 3.0})
	if a := flagged(got, "A"); a == nil || a.DeviationSigma > -3.0 {
		t.Fatalf("median/MAD must CATCH A despite the sock drag; got %+v", got)
	}
	// The socks (also genuinely low) are caught too — the attack backfires, not a false-negative.
	if flagged(got, "s1") == nil {
		t.Errorf("same-level socks should also be caught (they are low); got %+v", got)
	}
}

// common_mode (a cohort-WIDE drop, e.g. a model regression) is NEVER emitted: leave-one-out moves the
// OTHERS too, so no individual stands out.
func TestHardened_CommonModeNeverEmitted(t *testing.T) {
	var obs []keel.Observation
	// window 1: everyone high; window 2: EVERYONE drops ~0.30 together (spread keeps MAD>0).
	hi := []float64{0.82, 0.84, 0.85, 0.86, 0.88, 0.90}
	for i, q := range hi {
		obs = append(obs, hobs(u, string(rune('a'+i)), 1, q, 100))
		obs = append(obs, hobs(u, string(rune('a'+i)), 2, q-0.30, 100)) // all drop together
	}
	got := keel.DetectHardened(obs, keel.HardenedConfig{MoneyCohortFloor: 3, MinSamples: 1, PersistenceWindows: 1, HardenedSigma: 2.0})
	if len(got) != 0 {
		t.Errorf("common-mode cohort-wide drop must emit NOTHING hardened; got %+v", got)
	}
}

// Direction: a workspace robustly ABOVE its cohort (an upward spike) never emits.
func TestHardened_UpwardSpikeNotEmitted(t *testing.T) {
	var obs []keel.Observation
	for _, w := range []int64{1, 2} {
		obs = append(obs, hobs(u, "A", w, 0.99, 100)) // spike ABOVE
		for i, q := range []float64{0.50, 0.52, 0.55, 0.57} {
			obs = append(obs, hobs(u, string(rune('a'+i)), w, q, 100))
		}
	}
	got := keel.DetectHardened(obs, keel.HardenedConfig{MoneyCohortFloor: 3, MinSamples: 1, PersistenceWindows: 1, HardenedSigma: 2.0})
	if f := flagged(got, "A"); f != nil {
		t.Errorf("an upward spike must NOT emit (drop-only direction); got %+v", f)
	}
}

// Persistence: a single-window drop does NOT emit when PersistenceWindows > 1; a sustained drop does.
func TestHardened_PersistenceGating(t *testing.T) {
	peers := []float64{0.82, 0.84, 0.86, 0.88}
	build := func(dropWindows map[int64]bool) []keel.Observation {
		var obs []keel.Observation
		for _, w := range []int64{1, 2, 3} {
			aq := 0.85
			if dropWindows[w] {
				aq = 0.20
			}
			obs = append(obs, hobs(u, "A", w, aq, 100))
			for i, q := range peers {
				obs = append(obs, hobs(u, string(rune('a'+i)), w, q, 100))
			}
		}
		return obs
	}
	hc := func(k int) keel.HardenedConfig {
		return keel.HardenedConfig{MoneyCohortFloor: 4, MinSamples: 1, PersistenceWindows: k, HardenedSigma: 3.0}
	}
	// A drops only in the LAST window → persistence 3 requires all 3 → WITHHELD.
	only3 := build(map[int64]bool{3: true})
	if f := flagged(keel.DetectHardened(only3, hc(3)), "A"); f != nil {
		t.Errorf("single-window drop must be WITHHELD at persistence=3; got %+v", f)
	}
	// Same data at persistence=1 (only the last window matters) → emitted.
	if f := flagged(keel.DetectHardened(only3, hc(1)), "A"); f == nil {
		t.Error("the last-window drop must emit at persistence=1")
	}
	// A drops in all 3 windows → persistence 3 satisfied → emitted.
	all3 := build(map[int64]bool{1: true, 2: true, 3: true})
	if f := flagged(keel.DetectHardened(all3, hc(3)), "A"); f == nil {
		t.Error("a drop sustained across 3 windows must emit at persistence=3")
	}
}

// Money-grade floors + MAD==0 all WITHHOLD (no finding, no NaN/Inf).
func TestHardened_WithholdFloorsAndDegenerate(t *testing.T) {
	drop := func(others []float64, sample int) []keel.Observation {
		obs := []keel.Observation{hobs(u, "A", 1, 0.10, sample)}
		for i, q := range others {
			obs = append(obs, hobs(u, string(rune('a'+i)), 1, q, 100))
		}
		return obs
	}
	base := keel.HardenedConfig{MoneyCohortFloor: 10, MinSamples: 30, PersistenceWindows: 1, HardenedSigma: 2.0}

	// (a) below MoneyCohortFloor (only 4 others, floor 10) → withhold.
	if f := flagged(keel.DetectHardened(drop([]float64{0.8, 0.82, 0.85, 0.88}, 100), base), "A"); f != nil {
		t.Errorf("below MoneyCohortFloor must WITHHOLD; got %+v", f)
	}
	// (b) below MinSamples (sample 5 < 30) → withhold, even with a big cohort.
	big := []float64{0.80, 0.81, 0.82, 0.83, 0.84, 0.85, 0.86, 0.87, 0.88, 0.89, 0.90}
	if f := flagged(keel.DetectHardened(drop(big, 5), base), "A"); f != nil {
		t.Errorf("below MinSamples must WITHHOLD; got %+v", f)
	}
	// (c) MAD==0 (all 11 others identical) → withhold, no divide-by-zero / infinite score.
	identical := make([]float64, 11)
	for i := range identical {
		identical[i] = 0.80
	}
	got := keel.DetectHardened(drop(identical, 100), base)
	if f := flagged(got, "A"); f != nil {
		t.Errorf("MAD==0 degenerate cohort must WITHHOLD (no Inf); got %+v", f)
	}
}

// Determinism: unsorted input, byte-identical output across runs.
func TestHardened_Deterministic(t *testing.T) {
	var obs []keel.Observation
	order := []struct {
		ws string
		q  float64
	}{{"z", 0.30}, {"m", 0.85}, {"a", 0.88}, {"q", 0.82}, {"b", 0.87}, {"c", 0.84}}
	for _, w := range []int64{2, 1} { // windows out of order too
		for _, e := range order {
			obs = append(obs, hobs(u, e.ws, w, e.q, 100))
		}
	}
	hc := keel.HardenedConfig{MoneyCohortFloor: 3, MinSamples: 1, PersistenceWindows: 1, HardenedSigma: 3.0}
	first := keel.DetectHardened(obs, hc)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(first, keel.DetectHardened(obs, hc)) {
			t.Fatalf("DetectHardened is not deterministic on run %d", i)
		}
	}
}
