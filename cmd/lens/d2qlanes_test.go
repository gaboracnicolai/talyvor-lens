package main

import (
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// ⚠ THE CONSUMER HALF OF THE QUESTION HAD NO INSTRUMENT.
//
// W2.7 asks two questions in two populations — "of 30 engineering and 38 consumer rephrase pairs,
// how many are recalled via a variant?" and "of 44 + 42 danger pairs, how many are falsely
// recalled?" — and requires the same lane composition as cmd/hitrate "so the number sits directly
// beside W2.1's, W2.5's and W2.6's".
//
// This harness measured three engineering corpora and no consumer corpus at all. #393 then reported
// doc2query's verdict — "flat", "it does not pay" — over 68 pairs of which none were consumer
// traffic, beside three other items whose numbers were over 154. Nothing could see that, because
// the population was written inline in each program.
//
// ⚠ AND IT IS THE OMISSION DIRECTION, WHICH NO PER-LANE ASSERTION CATCHES. A check that every
// corpus measured is a real one passes trivially on three engineering corpora. The assertion that
// matters runs the other way: every pair poolsafety.Lanes() defines must appear in something this
// harness measures.
func TestD2QCorpora_MeasureEveryPairTheSharedLanesDefine(t *testing.T) {
	measured := map[string]bool{}
	for _, c := range d2qCorpora() {
		if len(c.pairs) == 0 {
			t.Errorf("corpus %q is empty — an empty corpus contributes no pairs and no error", c.name)
		}
		for _, p := range c.pairs {
			measured[p.A+"\x00"+p.B] = true
		}
	}

	missing := map[string]int{}
	total := 0
	for _, l := range poolsafety.Lanes() {
		for _, p := range l.Pairs {
			total++
			if !measured[p.A+"\x00"+p.B] {
				missing[l.Name()]++
			}
		}
	}
	if total == 0 {
		t.Fatal("poolsafety.Lanes() defines no pairs — the comparison below would pass vacuously")
	}
	for lane, n := range missing {
		t.Errorf("%s: %d pairs are in no corpus this harness measures — W2.7 asks for a number over "+
			"this lane and the harness cannot produce one", lane, n)
	}
}

// The reverse direction, so the two harnesses cannot drift the other way either: a pair measured
// here that no lane defines would be a figure with no counterpart in W2.1/W2.5/W2.6's tables.
func TestD2QCorpora_MeasureNothingTheSharedLanesDoNotDefine(t *testing.T) {
	defined := map[string]bool{}
	for _, l := range poolsafety.Lanes() {
		for _, p := range l.Pairs {
			defined[p.A+"\x00"+p.B] = true
		}
	}
	seen := 0
	for _, c := range d2qCorpora() {
		for _, p := range c.pairs {
			seen++
			if !defined[p.A+"\x00"+p.B] {
				t.Errorf("corpus %q measures %q, which is in no lane poolsafety.Lanes() defines",
					c.name, p.Name)
			}
		}
	}
	if seen == 0 {
		t.Fatal("d2qCorpora() yielded no pairs — every assertion above skipped")
	}
}
