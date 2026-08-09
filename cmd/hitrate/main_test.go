package main

import "testing"

// The report's headline numbers are produced by these four functions. A miscount here does not
// crash and does not look wrong — it prints a plausible table with the wrong number in it, which
// is the exact failure mode this whole item exists to catch. So they are pinned on a fixture whose
// every answer is countable by hand.
//
// The fixture is built so that each function has at least one pair that DISTINGUISHES it from the
// others: a pair that clears the threshold but fails the gate, a pair that fails the threshold but
// passes the gate, and a pair that passes the gate only because both sides are empty. A fixture
// where the counts coincide would pass under a function that returned the wrong one.
func fixture() []scored {
	return []scored{
		// clears 0.92, entities agree, non-empty  → sim-only AND production
		{name: "hit-both", sim: 0.9500, entityOK: true, canonA: "v:1", canonB: "v:1"},
		// clears 0.92, entities DISAGREE          → sim-only only (the entity gate's whole job)
		{name: "gate-refuses", sim: 0.9400, entityOK: false, canonA: "v:1", canonB: "v:2"},
		// clears 0.92, entities agree BY ABSENCE  → production, and counts as equal-because-empty
		{name: "empty-both", sim: 0.9300, entityOK: true, canonA: "", canonB: ""},
		// exactly ON 0.92 — the boundary is >=, so this one is IN
		{name: "boundary", sim: 0.9200, entityOK: true, canonA: "v:3", canonB: "v:3"},
		// below 0.92, entities agree              → neither column, but IS in the ceiling
		{name: "below-thresh", sim: 0.8000, entityOK: true, canonA: "v:4", canonB: "v:4"},
		// below 0.92, entities disagree           → nothing at all
		{name: "below-and-refused", sim: 0.7000, entityOK: false, canonA: "v:5", canonB: "v:6"},
		// ⚠ EXACTLY ONE SIDE EMPTY. Added because a positive control found the fixture blind
		// without it: with no such pair, `bothEmpty` written with || instead of && returns the same
		// answer as the correct version, and the test passes over a real defect. One side empty and
		// the other naming an entity is not agreement by absence — it is disagreement.
		{name: "one-side-empty", sim: 0.9100, entityOK: false, canonA: "", canonB: "v:7"},
	}
}

func TestCount(t *testing.T) {
	for _, tc := range []struct {
		thresh            float64
		wantSim, wantProd int
	}{
		// 0.98: nothing reaches it. A threshold that admits nothing must report zero in BOTH
		// columns, not "all of them" — an inverted comparison would show 6/6 here.
		{0.98, 0, 0},
		{0.95, 1, 1},
		// 0.92 is the shipped threshold and the boundary case: >= admits `boundary` at exactly
		// 0.9200. Four clear it; `gate-refuses` is dropped by the entity gate, so production is 3.
		{0.92, 4, 3},
		{0.70, 7, 4},
	} {
		gotSim, gotProd := count(fixture(), tc.thresh)
		if gotSim != tc.wantSim || gotProd != tc.wantProd {
			t.Errorf("count(t=%.2f) = (sim %d, prod %d), want (sim %d, prod %d)",
				tc.thresh, gotSim, gotProd, tc.wantSim, tc.wantProd)
		}
	}
}

// TestCountProductionNeverExceedsSimOnly pins the structural relationship rather than a number:
// production is a strict subset of sim-only at every threshold, because it is the same predicate
// plus one more conjunct. If this ever inverts, the two columns have been swapped somewhere and
// every table in the report reads backwards.
func TestCountProductionNeverExceedsSimOnly(t *testing.T) {
	for _, th := range []float64{0.99, 0.98, 0.95, 0.92, 0.88, 0.5, 0.0} {
		sim, prod := count(fixture(), th)
		if prod > sim {
			t.Errorf("at t=%.2f production %d exceeds sim-only %d — the columns are swapped", th, prod, sim)
		}
	}
}

func TestEntityCeiling(t *testing.T) {
	// Four of six agree on entities: hit-both, empty-both, boundary, below-thresh. The ceiling is
	// deliberately threshold-free — `below-thresh` counts even though no threshold serves it,
	// because the ceiling is what the gate would allow if similarity were perfect.
	if got, want := entityCeiling(fixture()), 4; got != want {
		t.Errorf("entityCeiling() = %d, want %d", got, want)
	}
}

func TestBothEmpty(t *testing.T) {
	// Exactly one pair is equal-because-empty. This must NOT count `hit-both`, whose sides agree
	// on a real entity — conflating the two is precisely the misreading §4 of the report exists to
	// prevent.
	if got, want := bothEmpty(fixture()), 1; got != want {
		t.Errorf("bothEmpty() = %d, want %d", got, want)
	}
}

// TestBothEmptyIsSubsetOfCeiling pins the invariant the report's "share of passes" column divides
// by: every equal-because-empty pair is also an entity-gate pass, so the ratio can never exceed 1.
func TestBothEmptyIsSubsetOfCeiling(t *testing.T) {
	f := fixture()
	if bothEmpty(f) > entityCeiling(f) {
		t.Errorf("bothEmpty %d > entityCeiling %d — the report would print a share above 100%%",
			bothEmpty(f), entityCeiling(f))
	}
}

func TestFrac(t *testing.T) {
	for _, tc := range []struct {
		n, d int
		want string
	}{
		{0, 38, "0/38 0%"},
		{2, 30, "2/30 7%"},
		{30, 38, "30/38 79%"},
		// A zero denominator is a real case: a lane with no danger pairs. It must read n/a rather
		// than divide by zero or silently print 0%, which would look like a clean result.
		{0, 0, "n/a"},
	} {
		if got := frac(tc.n, tc.d); got != tc.want {
			t.Errorf("frac(%d,%d) = %q, want %q", tc.n, tc.d, got, tc.want)
		}
	}
}
