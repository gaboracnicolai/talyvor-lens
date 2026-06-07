package poolroyalty

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func sampleThresholds() DetectorThresholds {
	return DetectorThresholds{
		VolumeMinMints:      50,
		VolumeMaxRequesters: 2,
		BilateralMinFrac:    0.9,
		BilateralMinMints:   20,
		SimilarityMinSample: 30,
		SimilarityMaxStddev: 0.02,
	}
}

// VOLUME flag rule: high total mints on an entry AND few distinct requesters =
// concentration. Many distinct requesters = legit popularity, never flagged.
func TestVolumeFlagged(t *testing.T) {
	th := sampleThresholds()
	cases := []struct {
		entryTotal, distinct int
		want                 bool
	}{
		{100, 1, true},   // one requester hammering a popular entry
		{100, 2, true},   // two — at the cap boundary
		{100, 50, false}, // genuinely popular: many distinct requesters
		{10, 1, false},   // below the volume floor
		{49, 1, false},   // just under
		{50, 2, true},    // exactly at both bounds
	}
	for _, c := range cases {
		if got := th.volumeFlagged(c.entryTotal, c.distinct); got != c.want {
			t.Errorf("volumeFlagged(total=%d, distinct=%d) = %v, want %v", c.entryTotal, c.distinct, got, c.want)
		}
	}
}

// BILATERAL flag rule: both sides' flow concentrated through the one
// counterparty, above a minimum volume.
func TestBilateralFlagged(t *testing.T) {
	th := sampleThresholds()
	cases := []struct {
		pairMints      int
		fracC, fracR   float64
		want           bool
	}{
		{100, 1.0, 1.0, true},   // fully bilateral
		{100, 0.95, 0.92, true}, // both above 0.9
		{100, 0.95, 0.5, false}, // requester spreads its flow → not concentrated
		{5, 1.0, 1.0, false},    // fully bilateral but below the volume floor
		{20, 0.9, 0.9, true},    // exactly at bounds
	}
	for _, c := range cases {
		if got := th.bilateralFlagged(c.pairMints, c.fracC, c.fracR); got != c.want {
			t.Errorf("bilateralFlagged(mints=%d, fracC=%v, fracR=%v) = %v, want %v", c.pairMints, c.fracC, c.fracR, got, c.want)
		}
	}
}

// SIMILARITY flag rule: enough sample, tight stddev (engineered cluster), AND a
// majority of DISTINCT prompts (organic re-asks repeat one prompt_sha256 →
// low distinct; engineered near-dupes are many different prompts at one
// similarity → high distinct).
func TestSimilarityFlagged(t *testing.T) {
	th := sampleThresholds()
	cases := []struct {
		hits, distinctPrompts int
		stddev                float64
		want                  bool
	}{
		{40, 38, 0.01, true},   // tight + mostly-distinct prompts = engineered
		{40, 40, 0.0, true},    // perfectly tight, all distinct
		{40, 38, 0.10, false},  // distinct but spread wide = organic-ish
		{40, 2, 0.01, false},   // tight but the SAME prompt re-asked = organic
		{20, 19, 0.01, false},  // below min sample (HAVING also guards, belt+braces)
	}
	for _, c := range cases {
		if got := th.similarityFlagged(c.hits, c.distinctPrompts, c.stddev); got != c.want {
			t.Errorf("similarityFlagged(hits=%d, distinct=%d, stddev=%v) = %v, want %v", c.hits, c.distinctPrompts, c.stddev, got, c.want)
		}
	}
}

// NEVER-AUTO-ACT, enforced at the TYPE LEVEL: DetectorReader's db handle must
// expose ONLY read methods (Query/QueryRow) — no Exec, no Begin, no
// SendBatch, no CopyFrom. A detector that cannot reach a write primitive
// cannot revoke, slash, or mutate any balance/claim/ledger row — the
// guarantee is a compile-time impossibility, not a convention. This test
// fails if anyone widens detectorDB with a write method.
func TestDetectorReader_NoWriteMethods(t *testing.T) {
	dbField, ok := reflect.TypeOf(DetectorReader{}).FieldByName("db")
	if !ok {
		t.Fatal("DetectorReader must hold its db via a 'db' field")
	}
	forbidden := []string{"Exec", "Begin", "BeginTx", "SendBatch", "CopyFrom", "Prepare"}
	for i := 0; i < dbField.Type.NumMethod(); i++ {
		name := dbField.Type.Method(i).Name
		for _, bad := range forbidden {
			if name == bad {
				t.Errorf("detectorDB exposes write method %q — a detector must be read-only by construction", name)
			}
		}
	}
	// And it must actually have the read methods it needs.
	for _, need := range []string{"Query", "QueryRow"} {
		if _, has := dbField.Type.MethodByName(need); !has {
			t.Errorf("detectorDB missing read method %q", need)
		}
	}
}

// Nil/zero reader is inert (mirrors MarginReader) — minting off ⇒ no rows ⇒
// detectors return empty without error.
func TestDetectorReader_NilSafe(t *testing.T) {
	var r *DetectorReader
	if v, err := r.VolumeConcentration(context.Background(), time.Hour); err != nil || v != nil {
		t.Errorf("nil VolumeConcentration = %v, %v; want nil, nil", v, err)
	}
	if v, err := r.BilateralConcentration(context.Background(), time.Hour); err != nil || v != nil {
		t.Errorf("nil BilateralConcentration = %v, %v; want nil, nil", v, err)
	}
	if v, err := r.SimilarityGaming(context.Background(), time.Hour); err != nil || v != nil {
		t.Errorf("nil SimilarityGaming = %v, %v; want nil, nil", v, err)
	}
}
