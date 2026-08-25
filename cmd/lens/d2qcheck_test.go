package main

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// ⚠ THE DENOMINATOR IS A CLAIM ABOUT A POPULATION, AND IT WAS A LITERAL.
//
// #393 reported doc2query's recall as "0/28 … 1/28" across the whole sweep. The denominator was
// typed into the format string when EngineeringRephrasePairs() held 28 pairs. The corpus has since
// grown to 30 and the literal did not move, so the sweep reports a rate over a population that is
// two pairs smaller than the one it measured — and nothing in the program can notice, because the
// number it prints is not derived from the slice it counted.
//
// This asserts the rendered denominator against the corpus the harness actually feeds the renderer.
// It is deliberately a check on the RENDERED BYTES rather than on a variable: the defect lived
// entirely in the gap between what was counted and what was printed, and an assertion on the count
// would have passed throughout.
// The danger fraction is OPTIONAL in this pattern on purpose: today the renderer prints a bare
// count there, and a pattern that required a denominator would simply not match the row, turning a
// wrong-population defect into a silent zero-row pass. Matching the current shape is what lets the
// assertion below name the missing denominator instead of skipping it.
var sweepRow = regexp.MustCompile(`^\s+(\d+)\s+@(\d\.\d+)\s+(\d+)/(\d+)\s+(\d+)(?:/(\d+))?\s*$`)

func TestRenderSweep_DenominatorsEqualTheCorpusItWasGiven(t *testing.T) {
	reph := resultsFor(poolsafety.EngineeringRephrasePairs())
	danger := resultsFor(append(append([]poolsafety.RephrasePair{},
		poolsafety.EngineeringDangerPairs()...), poolsafety.HeldOutDangerPairs()...))

	var buf bytes.Buffer
	renderSweep(&buf, reph, danger)

	rows := 0
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		m := sweepRow.FindStringSubmatch(string(line))
		if m == nil {
			continue
		}
		rows++
		if got, _ := strconv.Atoi(m[4]); got != len(reph) {
			t.Errorf("rephrase denominator = %d, want %d (the corpus the renderer was handed)\n  row: %q",
				got, len(reph), line)
		}
		if m[6] == "" {
			t.Errorf("danger column states no population at all — %q is a bare count, and W2.7 asks "+
				"how many OF 44+42 danger pairs are falsely recalled\n  row: %q", m[5], line)
			continue
		}
		if got, _ := strconv.Atoi(m[6]); got != len(danger) {
			t.Errorf("danger denominator = %d, want %d (the corpus the renderer was handed)\n  row: %q",
				got, len(danger), line)
		}
	}
	if want := len(sweepNs) * len(sweepThresholds); rows != want {
		t.Fatalf("parsed %d sweep rows, want %d — the row shape moved and every assertion above "+
			"silently stopped running", rows, want)
	}
}

// resultsFor builds one result per pair with a similarity nothing clears, so the counts are zero
// and only the denominators are under test.
func resultsFor(pairs []poolsafety.RephrasePair) []d2qResult {
	out := make([]d2qResult, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, d2qResult{Pair: p, Baseline: 0, Best: 0, GateAllows: true})
	}
	return out
}

// ⚠ THE PER-VARIANT SCORES WERE KEYED BY A NAME THAT IS NOT UNIQUE.
//
// measureCorpus stashed each pair's variant scores in a package-level map keyed by pair.Name, and
// runD2QCheck calls it once per corpus. Pair names are unique WITHIN a corpus and not across them:
// poolsafety has exactly one collision today — "notice-direction" appears in both
// ConsumerDangerPairs and ConsumerUnrelatedPairs, the two lanes W2.7 asks this harness to start
// measuring. The second corpus overwrites the first, and BestAtN then reports one lane's variant
// scores under the other lane's pair. The sweep row it feeds is wrong with no error anywhere.
//
// The collision is invisible today only because the harness measures three corpora that happen not
// to collide. Adding the lane the item asks for is what arms it.
func TestMeasureCorpus_SameNameInTwoCorporaKeepsItsOwnVariantScores(t *testing.T) {
	ctx := t.Context()
	emb := fakeEmbedder{vec: map[string][]float32{
		"l1a": {1, 0, 0}, "l1b": {0, 1, 0}, "l1 variant question?": {0, 1, 0}, // variant == B: 1.0000
		"l2a": {1, 0, 0}, "l2b": {0, 1, 0}, "l2 variant question?": {1, 0, 0}, // variant == A: 0.0000
	}}
	lane1 := []poolsafety.RephrasePair{{Name: "notice-direction", A: "l1a", B: "l1b"}}
	lane2 := []poolsafety.RephrasePair{{Name: "notice-direction", A: "l2a", B: "l2b"}}

	r1, err := measureCorpus(ctx, emb, fakeDeriver{q: "l1 variant question?"}, fakeDeriver{q: "l1 variant question?"}, lane1, 8)
	if err != nil {
		t.Fatalf("lane1: %v", err)
	}
	if _, err := measureCorpus(ctx, emb, fakeDeriver{q: "l2 variant question?"}, fakeDeriver{q: "l2 variant question?"}, lane2, 8); err != nil {
		t.Fatalf("lane2: %v", err)
	}

	// lane1's single variant is B itself, so its best-at-1 is 1.0. lane2's variant is A, worth
	// nothing. If lane2's measurement reached lane1's row, this reads ~0 instead.
	if got := r1[0].BestAtN(1); got < 0.99 {
		t.Errorf("lane1 BestAtN(1) = %.4f, want ~1.0000 — lane2's variant scores overwrote lane1's "+
			"because both pairs are named %q", got, r1[0].Pair.Name)
	}
}

type fakeEmbedder struct{ vec map[string][]float32 }

func (f fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := f.vec[text]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("fakeEmbedder: no vector for %q — the test must pin every text the "+
		"harness embeds, or a typo silently becomes a default vector", text)
}

type fakeDeriver struct{ q string }

func (f fakeDeriver) Derive(_ context.Context, _ string, _ int) ([]string, error) {
	return []string{f.q}, nil
}
