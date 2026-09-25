package poolsafety

import (
	"context"
	"math"
)

// SafetyFloor is B9.4: the LOWEST similarity threshold at which nothing the committed corpora say
// must never be served would be served. That covers every danger pair in ByTraffic() under the serving
// predicate (similarity ≥ t AND equal discriminators, the entity gate), and Corpus()'s unrelated
// preamble pairs by similarity alone (the original attestation). It returns the floor and the pair that
// sets it. entityMatch is the serving path's entity gate (discriminator.Match), passed in because
// discriminator's own tests import this package's corpora.
//
// Measured 2026-09-25 on text-embedding-3-small (docs/pool-b91-measured.md): the highest-scoring
// entity-equal danger pair is consumer `isa-year` at 0.9377 (floor 0.9377), so any live threshold at or below it
// serves a wrong answer. The attestation used to record whatever threshold was live when poolcheck
// ran (0.92), which admitted exactly that.
func SafetyFloor(ctx context.Context, emb Embedder, entityMatch func(a, b string) bool) (floor float64, pair string, score float64, err error) {
	var danger []RephrasePair
	for _, l := range ByTraffic() {
		danger = append(danger, l.Danger...)
	}
	scored, err := ScorePairs(ctx, emb, danger)
	if err != nil {
		return 0, "", 0, err
	}
	for _, s := range scored {
		if entityMatch(s.Pair.A, s.Pair.B) && s.Similarity > score {
			score, pair = s.Similarity, s.Pair.Name
		}
	}
	rep, err := Check(ctx, emb, 1)
	if err != nil {
		return 0, "", 0, err
	}
	if rep.Worst.Similarity > score {
		score, pair = rep.Worst.Similarity, rep.Worst.Pair.Name
	}
	return floorAbove(score), pair, score, nil
}

// floorAbove is the smallest 4-decimal threshold STRICTLY above score — a pair is served at
// similarity ≥ threshold, so a floor equal to the worst score would still serve it.
func floorAbove(score float64) float64 {
	f := math.Ceil(score*1e4) / 1e4
	if f <= score {
		f += 1e-4
	}
	return f
}
