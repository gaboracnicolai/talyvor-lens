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
// grown to 30 and the literal did not move, so the sweep reported a rate over a population two
// pairs smaller than the one it measured — and nothing in the program could notice, because the
// number it printed was not derived from the slice it counted.
//
// This asserts the rendered denominators against the corpora the harness actually feeds the
// renderer. It is deliberately a check on the RENDERED BYTES rather than on a variable: the defect
// lived entirely in the gap between what was counted and what was printed, and an assertion on the
// count would have passed throughout.
//
// ⚠ WHICH MAKES THE GUARD REGEX-SHAPED, AND THAT IS ITS OWN HAZARD. A row shape that moves matches
// nothing, every assertion inside the loop stops running, and the test passes by non-execution.
// The row-count assertion at the bottom is what makes that impossible; the control harness mutates
// the row shape specifically to prove it reds.
var sweepRow = regexp.MustCompile(
	`^\s+(\d+)\s+@(\d\.\d+)\s+(\d+)/(\d+)\s+(\d+)/(\d+)\s+\x{2502}\s+(\d+)/(\d+)\s+(\d+)/(\d+)\s*$`)

func TestRenderSweep_DenominatorsEqualTheCorpusItWasGiven(t *testing.T) {
	reph := resultsFor(laneNamed(t, "ENGINEERING rephrase"))
	danger := resultsFor(laneNamed(t, "ENGINEERING danger"))

	var buf bytes.Buffer
	renderSweep(&buf, "ENGINEERING", reph, danger)

	rows := 0
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		m := sweepRow.FindStringSubmatch(string(line))
		if m == nil {
			continue
		}
		rows++
		for _, c := range []struct {
			group int
			want  int
			what  string
		}{
			{4, len(reph), "HIT prod"}, {6, len(reph), "HIT sim"},
			{8, len(danger), "FALSE prod"}, {10, len(danger), "FALSE sim"},
		} {
			if got, _ := strconv.Atoi(m[c.group]); got != c.want {
				t.Errorf("%s denominator = %d, want %d (the corpus the renderer was handed)\n  row: %q",
					c.what, got, c.want, line)
			}
		}
	}
	if want := len(sweepNs) * len(sweepThresholds); rows != want {
		t.Fatalf("parsed %d sweep rows, want %d — the row shape moved and every assertion above "+
			"silently stopped running", rows, want)
	}
}

// The gate-allowed line is the sweep's binding number, so it is rendered and asserted rather than
// left to the reader: production recall cannot exceed it at any threshold or any variant count.
func TestRenderSweep_StatesTheGateAllowedCeiling(t *testing.T) {
	reph := resultsFor(laneNamed(t, "CONSUMER rephrase"))
	danger := resultsFor(laneNamed(t, "CONSUMER danger"))
	for i := range reph {
		reph[i].GateAllows = i < 3 // 3 of 38, chosen so it matches no other number on the line
	}
	for i := range danger {
		danger[i].GateAllows = false
	}

	var buf bytes.Buffer
	renderSweep(&buf, "CONSUMER", reph, danger)
	want := fmt.Sprintf("gate-allowed: rephrase 3/%d · danger 0/%d", len(reph), len(danger))
	if !bytes.Contains(buf.Bytes(), []byte(want)) {
		t.Errorf("sweep does not state its ceiling as %q\n---\n%s", want, buf.String())
	}
}

// laneNamed reads the population from the shared definition rather than re-deriving it: a guard
// over a corpus nothing reports on pins nothing.
func laneNamed(t *testing.T, name string) []poolsafety.RephrasePair {
	t.Helper()
	for _, l := range poolsafety.Lanes() {
		if l.Name() == name {
			return l.Pairs
		}
	}
	t.Fatalf("no lane named %q in poolsafety.Lanes()", name)
	return nil
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

	r1, err := measureCorpus(ctx, emb, fakeDeriver{q: "l1 variant question?"}, fakeDeriver{q: "l1 variant question?"}, lane1, 8, nil)
	if err != nil {
		t.Fatalf("lane1: %v", err)
	}
	if _, err := measureCorpus(ctx, emb, fakeDeriver{q: "l2 variant question?"}, fakeDeriver{q: "l2 variant question?"}, lane2, 8, nil); err != nil {
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
