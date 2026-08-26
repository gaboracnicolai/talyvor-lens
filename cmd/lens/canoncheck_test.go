package main

import (
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// wordDistance produces a NUMBER THAT GOES INTO THE REPORT — "of the 29 that did not collapse, N
// differ by exactly one word" is the sentence that separates "tighten the instruction" from "the
// approach cannot work". A distance function that is off by one turns a structural finding into a
// near-miss finding, so it is pinned against hand-counted cases rather than trusted.
func TestWordDistanceCountsWordsNotCharacters(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"what is the capital of france", "what is the capital of france", 0},
		{"what is the capital of france", "what is the capital of germany", 1},
		{"how much notice must a tenant give", "how much notice must a landlord give", 1},
		{"what is a goroutine", "what is a goroutine in go", 2},
		{"", "", 0},
		{"", "one two three", 3},
		{"one two three", "", 3},
		// ⚠ THE CASE THAT SEPARATES WORDS FROM CHARACTERS. Three letters differ and one word
		// does; a character-level distance would report 3 here and make every one-word miss look
		// structural.
		{"how do i profile memory", "how do i profile cpuxx", 1},
		// Reordering is not free: two words out of place is two edits, not zero.
		{"a b c", "c b a", 2},
	}
	for _, c := range cases {
		if got := wordDistance(c.a, c.b); got != c.want {
			t.Errorf("wordDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// Symmetry is not decoration: the harness passes (A, B) in corpus order, and a distance that
// depended on argument order would report different miss counts for the same corpus depending on
// which side of each pair happened to be listed first.
func TestWordDistanceIsSymmetric(t *testing.T) {
	pairs := [][2]string{
		{"how much paracetamol can i take", "how much paracetamol can a child take"},
		{"what is photosynthesis", "what is the definition of photosynthesis"},
		{"one", "one two three four five"},
	}
	for _, p := range pairs {
		if wordDistance(p[0], p[1]) != wordDistance(p[1], p[0]) {
			t.Errorf("asymmetric on %q/%q: %d vs %d", p[0], p[1],
				wordDistance(p[0], p[1]), wordDistance(p[1], p[0]))
		}
	}
}

// ⚠ W2.6 WROTE "notice-direction IS THE TEST THAT MATTERS" AND THE HARNESS SELECTED IT BY NAME
// FROM A POPULATION WHERE THAT NAME WAS NOT UNIQUE — AND BY A SECOND NAME THAT EXISTED NOWHERE.
//
// A by-name selector fails silently in both directions. Matching twice prints a second verdict
// block that is indistinguishable from the real one; matching zero times prints nothing, which is
// indistinguishable from a section that simply was not reached. Neither is an error.
//
// This asserts the resolution, not the intention: every name in namedTests must resolve to exactly
// one pair across the whole corpus the harness searches.
func TestNamedTestPairs_EachNameResolvesToExactlyOnePair(t *testing.T) {
	var everything []poolsafety.RephrasePair
	for _, l := range poolsafety.Lanes() {
		everything = append(everything, l.Pairs...)
	}
	if len(namedTests) == 0 {
		t.Fatal("namedTests is empty — the loop below would assert nothing")
	}
	for _, want := range namedTests {
		n := 0
		for _, p := range everything {
			if p.Name == want {
				n++
			}
		}
		switch {
		case n == 0:
			t.Errorf("namedTests entry %q matches NO pair in any corpus — the named test it "+
				"guards has never run, and a section that prints nothing looks the same as one "+
				"that was not reached", want)
		case n > 1:
			t.Errorf("namedTests entry %q matches %d different pairs — the named test runs %d "+
				"times and %d of those verdicts are for a pair W2.6 did not name", want, n, n, n-1)
		}
	}
	if got := len(namedTestPairs(everything)); got != len(namedTests) {
		t.Errorf("namedTestPairs returned %d pairs for %d names", got, len(namedTests))
	}
}
