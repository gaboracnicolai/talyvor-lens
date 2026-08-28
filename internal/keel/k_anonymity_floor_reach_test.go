package keel

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

// THE K-ANONYMITY FLOOR IS NOW BOUND TO A TEST. Nothing depended on its value.
//
// MEASURED 2026-08-28 (tab-k2w8, W4.36/W4.37) with a reach probe that mutates
// the constant and runs all 114 packages with internal/defaultsguard EXCLUDED —
// excluded because that guard reds on any change to a pinned constant and would
// otherwise make every floor read as enforced
// (~/talyvor-queue/w436-reach-probe-k2w8.py).
//
// RESULT: DefaultMinWorkspaces 3 -> 1 was noticed by NOTHING, with the probe
// positive-controlled 3/3.
//
// ⚠ WHY, AND IT IS THE SAME SHAPE FOUND IN poolroyalty THE SAME DAY: this
// package's tests are thorough, and EVERY ONE of them passes the LITERAL 3 —
// keel_test.go's cfg2(), keel_hardened_test.go twice, keel_breach_integration_test.go.
// The detector is well tested; the CONSTANT is what nothing bound. A test that
// hardcodes the threshold it is testing cannot notice that threshold changing.
//
// ⚠ WHAT WAS NOT WRONG: the constant IS wired — DefaultConfig() sets it,
// Detect() falls back to it when handed <= 0, keel_hardened raises
// MoneyCohortFloor to it, and cmd/lens/main.go builds its keel.Config from it.
// (Named without a line number deliberately. The first draft of this comment
// carried one and internal/pointeraudit refused it: a line citation decays with
// no commit in the file it points at, which is exactly why that census exists.)
// Nothing was inert. What was missing was any test whose OUTCOME depended on
// its value.
//
// ⚠ WHY THE VALUE ASSERTION IS A LOWER BOUND AND NOT AN EQUALITY, stated
// because the difference is the whole design: an equality (== 3) would be a
// second copy of the literal, and raising the floor to 4 — strictly MORE
// private — would red it. The bound asserted here is >= 3, and it is derived
// from a privacy fact rather than from the shipped number: a cohort of 2 is
// INVERTIBLE. TestACohortOfTwoIsInvertible executes that inversion, so the
// bound below is justified by a computation in this file rather than asserted.

// TestACohortOfTwoIsInvertible is the justification for the floor's lower bound.
// It asserts nothing about keel; it demonstrates arithmetic. With two members,
// a member who knows the cohort mean and its own value recovers the OTHER
// member's value exactly — so a 2-cohort aggregate is not an aggregate.
func TestACohortOfTwoIsInvertible(t *testing.T) {
	mine, theirs := 0.8, 0.2
	cohortMean := (mine + theirs) / 2
	recovered := 2*cohortMean - mine // what any member can compute
	if math.Abs(recovered-theirs) > 1e-12 {
		t.Fatalf("recovered %v, want %v — if this fails the arithmetic below is wrong, not keel", recovered, theirs)
	}
}

// TestKAnonymityFloorIsAtLeastThree is the assertion the reach probe's mutation
// trips. It permits any value >= 3, so tightening the floor stays green.
func TestKAnonymityFloorIsAtLeastThree(t *testing.T) {
	const invertibleCohort = 2 // see TestACohortOfTwoIsInvertible
	if DefaultMinWorkspaces <= invertibleCohort {
		t.Fatalf("DefaultMinWorkspaces = %d, which permits a cohort of %d or smaller. "+
			"A 2-member cohort mean is invertible by subtraction (proved in "+
			"TestACohortOfTwoIsInvertible), so any member recovers the other member's raw "+
			"value exactly — the aggregate stops being an aggregate. The floor must stay > %d.",
			DefaultMinWorkspaces, DefaultMinWorkspaces, invertibleCohort)
	}
}

// TestDefaultConfigWiresTheFloor covers the other half of "enforced": that the
// constant reaches the Config production builds. cmd/lens/main.go constructs
// keel.Config{MinWorkspaces: keel.DefaultMinWorkspaces}, and DefaultConfig is
// the in-package equivalent.
func TestDefaultConfigWiresTheFloor(t *testing.T) {
	if got := DefaultConfig().MinWorkspaces; got != DefaultMinWorkspaces {
		t.Errorf("DefaultConfig().MinWorkspaces = %d, want DefaultMinWorkspaces = %d — "+
			"the pinned constant would not be the floor the detector applies", got, DefaultMinWorkspaces)
	}
}

// driftScenario builds n workspaces over two windows with ws00 drifting in the
// current window — the shape keel_test.go already uses, sized by the caller.
func driftScenario(n int) []Observation {
	var in []Observation
	var wss []string
	for i := 0; i < n; i++ {
		wss = append(wss, fmt.Sprintf("ws%02d", i))
	}
	for _, ws := range wss {
		in = append(in, Observation{Unit: "u", WorkspaceID: ws, Window: 1, MeanQuality: 0.8, Sample: 10})
	}
	in = append(in, Observation{Unit: "u", WorkspaceID: wss[0], Window: 2, MeanQuality: 0.2, Sample: 10})
	for _, ws := range wss[1:] {
		in = append(in, Observation{Unit: "u", WorkspaceID: ws, Window: 2, MeanQuality: 0.8, Sample: 10})
	}
	return in
}

// TestDetectFloorIsConsultedAndDefaultsToTheConstant pins the two behaviours
// Detect owes the floor, with a scenario that genuinely emits so neither
// assertion is vacuous.
//
// ⚠ The cohort is deliberately DefaultMinWorkspaces+3, not the floor itself:
// measured, this scenario only reaches 2σ at n >= 6, so a boundary-sized cohort
// would be withheld by the STATISTICS and the test would credit the floor for
// an outcome the floor did not cause.
func TestDetectFloorIsConsultedAndDefaultsToTheConstant(t *testing.T) {
	n := DefaultMinWorkspaces + 3
	in := driftScenario(n)

	emitted := Detect(in, DefaultConfig())
	if len(emitted) == 0 {
		t.Fatal("the drift scenario emits nothing under DefaultConfig() — every assertion " +
			"below would then be vacuously satisfied by a detector that never speaks")
	}

	// The floor is consulted: raise it above the cohort and the finding is withheld.
	if got := Detect(in, Config{MinWorkspaces: n + 1, DeviationSigma: DefaultConfig().DeviationSigma}); len(got) != 0 {
		t.Errorf("a cohort of %d survived a floor of %d — the MinWorkspaces floor is not applied; "+
			"got %d findings", n, n+1, len(got))
	}

	// The zero-value fallback resolves to the constant, not to some other number.
	viaFallback := Detect(in, Config{MinWorkspaces: 0, DeviationSigma: DefaultConfig().DeviationSigma})
	viaConstant := Detect(in, Config{MinWorkspaces: DefaultMinWorkspaces, DeviationSigma: DefaultConfig().DeviationSigma})
	if !reflect.DeepEqual(viaFallback, viaConstant) {
		t.Errorf("Detect with MinWorkspaces=0 did not fall back to DefaultMinWorkspaces=%d:\n"+
			" fallback: %+v\n constant: %+v", DefaultMinWorkspaces, viaFallback, viaConstant)
	}
}
