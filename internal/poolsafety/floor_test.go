package poolsafety

import "testing"

// A pair is served at similarity ≥ threshold, so the floor must sit STRICTLY above the worst score.
func TestFloorAbove_IsStrictlyAboveTheWorstScore(t *testing.T) {
	for score, want := range map[float64]float64{0.9377: 0.9378, 0.93771: 0.9378, 0.6534634880828601: 0.6535} {
		if got := floorAbove(score); got <= score || got-want > 1e-9 || want-got > 1e-9 {
			t.Errorf("floorAbove(%v) = %v, want %v", score, got, want)
		}
	}
}

// B9.4's control, both ways: with the floor today's corpora set (isa-year at 0.93766… → 0.9377) attested,
// a live 0.92 turns pooling off at boot and the live 0.98 keeps it on.
func TestAttestedFloor_RefusesALowerLiveThreshold_AdmitsTheDeclaredOne(t *testing.T) {
	a := Attestation{EmbeddingModel: "text-embedding-3-small", Threshold: floorAbove(0.93766), WorstPair: "isa-year", WorstScore: 0.93766}
	if ok, why := a.MatchesLive("text-embedding-3-small", 0.92); ok {
		t.Error("live 0.92 is below the floor and must turn pooling off")
	} else if why == "" {
		t.Error("the refusal must say why")
	}
	if ok, why := a.MatchesLive("text-embedding-3-small", 0.98); !ok {
		t.Errorf("live 0.98 is above the floor and must serve: %s", why)
	}
}
