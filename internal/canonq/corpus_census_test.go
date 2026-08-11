package canonq

import (
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// ⚠ THE NUMBERS IN cmd/lens/canoncheck.go's HEADER ARE FRACTIONS, AND A FRACTION IS A CLAIM ABOUT
// ITS DENOMINATOR. "1/30 collapsed" is only comparable to W2.1's and W2.5's figures while the
// lanes hold the same pairs they held when it was measured. A pair added to a corpus silently
// re-bases every recorded result in this repository and nothing would say so.
//
// ⚠ THE DENOMINATORS ARE HARDCODED LITERALS, NOT len() OF THE THING UNDER TEST. Comparing a
// corpus to an expression derived from that same corpus passes for every value it could ever have.
func TestLaneDenominatorsAreStillTheOnesTheReportWasMeasuredOn(t *testing.T) {
	got := map[string]int{
		"ENGINEERING rephrase": len(poolsafety.EngineeringRephrasePairs()),
		"ENGINEERING danger":   len(poolsafety.EngineeringDangerPairs()) + len(poolsafety.HeldOutDangerPairs()),
		"CONSUMER rephrase":    len(poolsafety.RephrasePairs()) + len(poolsafety.ConsumerRephrasePairs()),
		"CONSUMER danger":      len(poolsafety.ConsumerDangerPairs()) + len(poolsafety.ConsumerUnrelatedPairs()),
	}
	want := map[string]int{
		"ENGINEERING rephrase": 30,
		"ENGINEERING danger":   44,
		"CONSUMER rephrase":    38,
		"CONSUMER danger":      42,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s is now %d pairs, not %d — every figure in cmd/lens/canoncheck.go's header "+
				"is a fraction over the old denominator. Re-run `lens canoncheck` and update it, or "+
				"the recorded result is about a corpus that no longer exists.", k, got[k], w)
		}
	}
}

// ⚠ THE TWO PAIRS THAT COLLAPSED ARE NOT INTERCHANGEABLE, AND THE REPORT WOULD READ AS IF THEY
// WERE. `timeout-spacing` is "30s" against "30 s" — a TYPOGRAPHIC pair, the class W2.5 already
// measured tier-1 folding against. Counting it beside `capital-uk`, which is a genuine
// rephrasing, turns "one semantic rephrasing in 68" into "two", and the whole item is about
// whether semantic rephrasings collapse.
//
// This pins what those pairs ARE, so a future reader cannot take the raw count at face value.
func TestTheSpacingPairsAreTypographicNotSemantic(t *testing.T) {
	want := map[string][2]string{
		"heap-spacing":    {"How do I set the JVM heap to 512mb?", "How do I set the JVM heap to 512 mb?"},
		"timeout-spacing": {"Why does my request time out after 30s?", "Why does my request time out after 30 s?"},
	}
	found := 0
	for _, p := range poolsafety.EngineeringRephrasePairs() {
		w, ok := want[p.Name]
		if !ok {
			continue
		}
		found++
		if p.A != w[0] || p.B != w[1] {
			t.Errorf("%s changed: A=%q B=%q — canoncheck's header calls this pair typographic", p.Name, p.A, p.B)
		}
		// The two sides differ ONLY by whitespace. If that stops being true the pair has become a
		// semantic one and the header's arithmetic changes.
		if strings.Join(strings.Fields(p.A), "") != strings.Join(strings.Fields(p.B), "") {
			t.Errorf("%s is no longer whitespace-only: %q vs %q", p.Name, p.A, p.B)
		}
	}
	if found != len(want) {
		t.Fatalf("found %d of %d named spacing pairs — a pair the header reasons about has been "+
			"renamed or removed, so the reasoning no longer attaches to anything", found, len(want))
	}
}
