package poolsafety

import (
	"context"
	"strings"
	"testing"
)

// These guard the CLAIMS the harness makes, not the numbers it produced on one day. The numbers
// belong to (embedding model × corpus) and will move; what must not move is that the harness
// measures the pooled path, that its corpus is honest, and that its inversion detector can both
// fire and stay silent.

// recordingEmbedder returns fixed vectors and records exactly what text it was asked to embed.
// The recording is the point: the harness's central claim is about WHICH TEXT reaches the embedder.
type recordingEmbedder struct {
	seen []string
	vecs map[string][]float32
}

func (r *recordingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	r.seen = append(r.seen, text)
	if v, ok := r.vecs[text]; ok {
		return v, nil
	}
	return []float32{1, 0, 0}, nil
}

// ⚠ THE COMPARABILITY CLAIM. MeasureRephrase exists to predict the POOLED read, which embeds the
// raw prompt. If it ever embedded "<workspaceID>:<prompt>" like the PRIVATE read does, every score
// would be inflated by shared text and the report would overstate the pool's hit rate — the exact
// error the measurement was built to expose.
func TestMeasureRephrase_EmbedsRawPromptsOnly(t *testing.T) {
	rec := &recordingEmbedder{}
	if _, err := MeasureRephrase(context.Background(), rec, "m", 0.92); err != nil {
		t.Fatalf("MeasureRephrase: %v", err)
	}
	if len(rec.seen) == 0 {
		t.Fatal("embedded nothing — the measurement would report a vacuous distribution")
	}
	want := map[string]bool{}
	for _, p := range RephrasePairs() {
		want[p.A], want[p.B] = true, true
	}
	for _, got := range rec.seen {
		if strings.Contains(got, ":") && !want[got] {
			t.Errorf("embedded %q — looks workspace-prefixed. The pooled read embeds the RAW prompt; "+
				"prefixing would inflate every score with text no rephrasing can vary.", got)
		}
		if !want[got] {
			t.Errorf("embedded %q, which is not a corpus string", got)
		}
	}
}

// ⚠ THE PREFIX-LIFT MEASUREMENT MUST ACTUALLY PREFIX. If it embedded raw text on both sides the
// lift would be a flat zero and would read as "the two paths are equivalent" — a false all-clear.
func TestMeasurePrefixLift_EmbedsBothRawAndPrefixed(t *testing.T) {
	rec := &recordingEmbedder{}
	if _, err := MeasurePrefixLift(context.Background(), rec, "ws-abc", 0.92); err != nil {
		t.Fatalf("MeasurePrefixLift: %v", err)
	}
	var raw, prefixed int
	for _, s := range rec.seen {
		if strings.HasPrefix(s, "ws-abc:") {
			prefixed++
		} else {
			raw++
		}
	}
	if prefixed == 0 {
		t.Error("no prefixed text embedded — the lift would be identically zero and would read as " +
			"'private and pooled are the same measurement', which is the belief this disproves")
	}
	if raw == 0 {
		t.Error("no raw text embedded — nothing to compare the prefixed scores against")
	}
	if raw != prefixed {
		t.Errorf("raw=%d prefixed=%d — every pair must be scored both ways", raw, prefixed)
	}
}

func scores(sims ...float64) []PairScore {
	out := make([]PairScore, 0, len(sims))
	for i, s := range sims {
		out = append(out, PairScore{Pair: RephrasePair{Name: string(rune('a' + i))}, Similarity: s})
	}
	return out
}

// ⚠ THE INVERSION DETECTOR MUST FIRE when the best genuine rephrasing scores below the worst
// dangerous pair — the state that means no threshold can serve anyone safely.
func TestSeparationTable_ReportsInversion(t *testing.T) {
	out := SeparationTable(scores(0.60, 0.87), scores(0.55, 0.89))
	if !strings.Contains(out, "INVERTED") {
		t.Errorf("best rephrasing 0.87 < worst dangerous 0.89 and the table did not say INVERTED.\n"+
			"Without it the report reads as 'hit rate is low', inviting a threshold cut that would "+
			"admit wrong answers before it served anybody.\ngot:\n%s", out)
	}
}

// ⚠ AND IT MUST BE ABLE TO STAY SILENT. A detector that always fires proves nothing — this is the
// positive control for the test above. If both pass, the message tracks the data.
func TestSeparationTable_SilentWhenPopulationsSeparate(t *testing.T) {
	out := SeparationTable(scores(0.94, 0.96), scores(0.55, 0.60))
	if strings.Contains(out, "INVERTED") {
		t.Errorf("rephrasings (0.94, 0.96) sit cleanly above the dangerous pairs (0.55, 0.60), which "+
			"is the healthy state, yet the table cried INVERTED. A warning that cannot be absent "+
			"carries no information.\ngot:\n%s", out)
	}
}

// The separation table's counts are the whole trade-off. A miscount would misprice the decision.
func TestSeparationTable_CountsServedAndAdmitted(t *testing.T) {
	out := SeparationTable(scores(0.86, 0.93), scores(0.91, 0.99))
	if !strings.Contains(out, "0.92") {
		t.Fatalf("table omitted the 0.92 row:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "0.92") {
			// At 0.92: one rephrasing (0.93) served, one dangerous pair (0.99) admitted.
			if !strings.Contains(line, " 1 ") {
				t.Errorf("0.92 row should show 1 served and 1 admitted, got: %q", line)
			}
		}
	}
}

// ⚠ CORPUS HONESTY. A pair whose two sides are the same string is not a rephrasing; it would score
// 1.0 and lift the reported hit rate for free. The brief asked for 20–30 realistic pairs.
func TestRephrasePairs_AreDistinctAndSizedAsSpecified(t *testing.T) {
	pairs := RephrasePairs()
	if len(pairs) < 20 || len(pairs) > 30 {
		t.Errorf("corpus has %d pairs, want 20–30", len(pairs))
	}
	seen := map[string]bool{}
	for _, p := range pairs {
		if p.A == p.B {
			t.Errorf("%s: both sides identical — scores 1.0 and inflates the hit rate for free", p.Name)
		}
		if seen[p.Name] {
			t.Errorf("duplicate pair name %q", p.Name)
		}
		seen[p.Name] = true
	}
}

// ⚠ THE POSITIVE CONTROL MUST BE A REAL CONTROL. If the "identical" control's two sides ever drift
// apart it stops proving the embedder works, and a zero hit rate could then be a broken client
// rather than a finding — the difference between "the pool serves nobody" and "the run is void".
func TestPositiveControls_IdenticalPairIsGenuinelyIdentical(t *testing.T) {
	var found bool
	for _, p := range PositiveControls() {
		if p.Name == "identical" {
			found = true
			if p.A != p.B {
				t.Errorf("the 'identical' control is not identical (%q vs %q); it can no longer "+
					"distinguish a broken embedder from a true zero hit rate", p.A, p.B)
			}
		}
	}
	if !found {
		t.Error("no 'identical' positive control — the harness's numbers become unfalsifiable")
	}
}

// ⚠ THE DANGEROUS CORPUS MUST BE DANGEROUS: near-identical wording, different correct answer. A
// pair of plainly unrelated sentences would score low, flatter the ceiling, and overstate headroom.
func TestConsumerUnrelatedPairs_ShareMostOfTheirWording(t *testing.T) {
	for _, p := range ConsumerUnrelatedPairs() {
		a, b := strings.Fields(strings.ToLower(p.A)), strings.Fields(strings.ToLower(p.B))
		common := map[string]bool{}
		for _, w := range a {
			common[w] = true
		}
		var shared int
		for _, w := range b {
			if common[w] {
				shared++
			}
		}
		longer := len(a)
		if len(b) > longer {
			longer = len(b)
		}
		if float64(shared)/float64(longer) < 0.4 {
			t.Errorf("%s: only %d/%d words shared — too obviously unrelated to test the hazard, "+
				"which is a near-identical question with a DIFFERENT correct answer", p.Name, shared, longer)
		}
		if p.A == p.B {
			t.Errorf("%s: identical sides — this pair asserts nothing", p.Name)
		}
	}
}
