package poolsafety

import "testing"

// ⚠ THE FOUR POPULATION SIZES ARE PINNED AS LITERALS ON PURPOSE, AND THEY ARE THE ONLY LITERALS
// HERE. W2.1, W2.5, W2.6 and W2.7 each state their result as a fraction of "30 engineering and 38
// consumer rephrase pairs, 44 + 42 danger pairs". Those numbers are load-bearing prose in four
// queue items and three commit messages. If a corpus grows, this test fails and the person who
// grew it has to decide whether the published fractions are still comparable — which is exactly
// the decision #393 did not get to make when EngineeringRephrasePairs went 28 → 30 and a hard-coded
// denominator in cmd/lens d2qcheck kept reporting /28.
func TestLanes_PopulationsMatchThePublishedFigures(t *testing.T) {
	want := map[string]int{
		"ENGINEERING rephrase": 30,
		"ENGINEERING danger":   44,
		"CONSUMER rephrase":    38,
		"CONSUMER danger":      42,
	}
	got := map[string]int{}
	for _, l := range Lanes() {
		got[l.Name()] = len(l.Pairs)
	}
	if len(got) != len(want) {
		t.Fatalf("Lanes() returned %d lanes, want %d: %v", len(got), len(want), got)
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s = %d pairs, want %d — every published recall figure is a fraction of this "+
				"number; if the corpus grew on purpose, the figures in W2.1/W2.5/W2.6/W2.7 and their "+
				"commit messages are no longer comparable and must be re-measured, not re-labelled",
				name, got[name], n)
		}
	}
}

// The lane is a UNION, and a union is where a corpus goes missing silently: drop one source and
// the lane still has pairs, still reports a plausible rate, and is simply smaller. Asserting the
// sum against its named parts is what makes the omission visible.
func TestLanes_EachLaneIsExactlyItsNamedSources(t *testing.T) {
	byName := map[string][]RephrasePair{
		"EngineeringRephrasePairs": EngineeringRephrasePairs(),
		"EngineeringDangerPairs":   EngineeringDangerPairs(),
		"HeldOutDangerPairs":       HeldOutDangerPairs(),
		"RephrasePairs":            RephrasePairs(),
		"ConsumerRephrasePairs":    ConsumerRephrasePairs(),
		"ConsumerDangerPairs":      ConsumerDangerPairs(),
		"ConsumerUnrelatedPairs":   ConsumerUnrelatedPairs(),
	}
	used := map[string]bool{}
	for _, l := range Lanes() {
		if len(l.Sources) == 0 {
			t.Errorf("%s names no sources — a lane whose provenance is unstated cannot be checked "+
				"against the item that asked for it", l.Name())
			continue
		}
		var want []RephrasePair
		for _, s := range l.Sources {
			c, ok := byName[s]
			if !ok {
				t.Fatalf("%s names source %q, which is not a corpus in this package", l.Name(), s)
			}
			used[s] = true
			want = append(want, c...)
		}
		if len(l.Pairs) != len(want) {
			t.Errorf("%s has %d pairs but its sources %v sum to %d", l.Name(), len(l.Pairs), l.Sources, len(want))
			continue
		}
		for i := range want {
			if l.Pairs[i] != want[i] {
				t.Errorf("%s pair %d = %q, want %q (from %v)", l.Name(), i, l.Pairs[i].Name, want[i].Name, l.Sources)
				break
			}
		}
	}
	// ⚠ THE OTHER DIRECTION, WHICH IS THE ONE THAT ACTUALLY BIT. Every assertion above is about
	// lanes that EXIST. #393's harness was wrong by OMISSION — it measured three corpora and left
	// the consumer ones out entirely, and no per-lane check can see a lane that was never built.
	for name := range byName {
		if !used[name] {
			t.Errorf("corpus %s is in no lane — a corpus nothing measures is a population silently "+
				"excluded from every published figure", name)
		}
	}
}

// ⚠ THE FIRST VERSION OF THIS GUARD COULD NOT FAIL, AND ONLY THE POSITIVE CONTROL SAID SO.
//
// It built every lane three times and required EngineeringDangerPairs() unchanged. Rewriting
// union() to `out := corpora[0]; append(out, ...)` — the exact aliasing bug it was written for —
// left it GREEN, because each corpus function returns a fresh slice literal whose len equals its
// cap, so append always reallocates and the write it was watching for can never happen through
// that door. A test over inputs where the defect is unrepresentable is not a guard.
//
// This drives union() directly with a slice that HAS spare capacity, which is where appending onto
// a caller's slice actually clobbers, and then mutates the result and requires the input intact.
func TestUnion_CopiesRatherThanAppendingIntoItsInput(t *testing.T) {
	base := make([]RephrasePair, 1, 4) // cap > len: append would write into base's array
	base[0] = RephrasePair{Name: "first", A: "a", B: "b"}

	got := union(base, []RephrasePair{{Name: "second", A: "c", B: "d"}})
	if len(got) != 2 {
		t.Fatalf("union returned %d pairs, want 2", len(got))
	}
	if len(base) != 1 {
		t.Errorf("union grew its input to %d — a corpus that changes length as lanes are built is "+
			"a different population for whoever reads it next", len(base))
	}

	got[0] = RephrasePair{Name: "clobbered"}
	if base[0].Name != "first" {
		t.Errorf("writing to the union changed the input corpus: base[0] = %q, want %q — union "+
			"returned a view of its caller's array, not a copy", base[0].Name, "first")
	}
}

// ⚠ A NAME IS HOW A PAIR IS IDENTIFIED IN EVERY REPORT THIS REPO PRINTS, AND ONE NAME IS USED BY
// TWO DIFFERENT PAIRS.
//
// "notice-direction" is a landlord-notice pair in ConsumerDangerPairs AND an employment-notice
// pair in ConsumerUnrelatedPairs. The CONSUMER danger lane unions those two corpora, so one lane
// holds two different pairs under one label.
//
// It is not cosmetic. cmd/lens canoncheck selects W2.6's "THE NAMED TEST" by that string over
// exactly this union, with no break — so it runs the named test twice, once on a pair W2.6 never
// named. And cmd/lens d2qcheck kept per-variant scores in a map keyed by the name, which is how
// one lane's measurement reached another lane's row.
func TestLanes_PairNamesAreUniqueWithinALane(t *testing.T) {
	for _, l := range Lanes() {
		seen := map[string]RephrasePair{}
		for _, p := range l.Pairs {
			if prev, dup := seen[p.Name]; dup {
				t.Errorf("%s: two different pairs are both named %q\n  %q → %q\n  %q → %q",
					l.Name(), p.Name, prev.A, prev.B, p.A, p.B)
			}
			seen[p.Name] = p
		}
	}
}

// ⚠ THE CONSUMER DANGER LANE COUNTS ONE PAIR TWICE, AND EVERY CONSUMER PRECISION FIGURE THIS
// PROJECT HAS PUBLISHED IS A FRACTION OF THE INFLATED NUMBER.
//
// ConsumerDangerPairs holds {"dose-adult-child", "How much paracetamol can I take?", "How much
// paracetamol can a child take?"} and ConsumerUnrelatedPairs holds the byte-identical pair under
// the name "dose-who". The lane unions them, so a population reported as 42 is 41 DISTINCT pairs
// with one counted twice — and that single danger case carries double weight in W2.1's, W2.5's,
// W2.6's and W2.7's false-serve rates.
//
// ⚠ IT IS PINNED, NOT DELETED. Removing a fixture moves a denominator four published tables are
// stated over; that is a decision about the corpus, not a measurement, and W2.7's instruction is
// to measure and report. Pinning both counts puts the discrepancy on the record, stops it growing
// unnoticed, and stops it being quietly absorbed by whoever edits a corpus next.
func TestLanes_RawAndDistinctPopulationsAreBothPinned(t *testing.T) {
	want := map[string][2]int{ // lane → {raw, distinct}
		"ENGINEERING rephrase": {30, 30},
		"ENGINEERING danger":   {44, 44},
		"CONSUMER rephrase":    {38, 38},
		// ⚠ 42 ROWS, 41 DISTINCT PAIRS. dose-adult-child (ConsumerDangerPairs) and dose-who
		// (ConsumerUnrelatedPairs) are byte-identical, and this lane unions both corpora.
		"CONSUMER danger": {42, 41},
	}
	for _, l := range Lanes() {
		distinct := map[string]bool{}
		for _, p := range l.Pairs {
			distinct[p.A+"\x00"+p.B] = true
		}
		w, ok := want[l.Name()]
		if !ok {
			t.Errorf("lane %q is not pinned here — a new lane's population must be stated before "+
				"any figure is published over it", l.Name())
			continue
		}
		if len(l.Pairs) != w[0] || len(distinct) != w[1] {
			t.Errorf("%s = %d pairs / %d distinct, want %d / %d — if a corpus gained or lost a pair, "+
				"or gained a duplicate, every published fraction over this lane needs re-measuring, "+
				"not re-labelling", l.Name(), len(l.Pairs), len(distinct), w[0], w[1])
		}
	}
}

// ByTraffic is what three programs now build their sections from, so a lane that arrives empty
// there is a whole population silently dropped from a printed report — the omission direction
// again, one level up from Lanes().
func TestByTraffic_EveryTrafficHasBothSidesAndNothingIsLost(t *testing.T) {
	got := ByTraffic()
	if len(got) != 2 {
		t.Fatalf("ByTraffic returned %d entries, want 2", len(got))
	}
	total := 0
	for _, tl := range got {
		if len(tl.Rephrase) == 0 || len(tl.Danger) == 0 {
			t.Errorf("%s has %d rephrase and %d danger pairs — an empty side prints a section with "+
				"no rows, which reads as 'nothing to report' rather than 'nothing was measured'",
				tl.Traffic, len(tl.Rephrase), len(tl.Danger))
		}
		total += len(tl.Rephrase) + len(tl.Danger)
	}
	want := 0
	for _, l := range Lanes() {
		want += len(l.Pairs)
	}
	if total != want {
		t.Errorf("ByTraffic carries %d pairs, Lanes() defines %d — the grouping dropped %d",
			total, want, want-total)
	}
}
