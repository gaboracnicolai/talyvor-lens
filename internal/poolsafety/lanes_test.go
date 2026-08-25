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
