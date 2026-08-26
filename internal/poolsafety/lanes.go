package poolsafety

// LANE COMPOSITION — ONE DEFINITION, BECAUSE EVERY RECALL NUMBER THIS PROJECT HAS PUBLISHED IS
// COMPARED AGAINST THE OTHERS.
//
// W2.1, W2.5 and W2.6 each reported a hit rate "on 30 engineering and 38 consumer rephrase pairs,
// 44 + 42 danger pairs". cmd/hitrate built that composition inline; cmd/lens d2qcheck built a
// DIFFERENT one inline — three engineering corpora and no consumer lane at all — and #393's
// doc2query verdict was therefore reported against a population that does not overlap the one the
// other three items used, with nothing in either program able to say so.
//
// ⚠ THE POPULATION IS THE COMPARISON. Two instruments reporting "1/28" and "1/30" over different
// unions are not disagreeing about a mechanism, they are answering different questions; a reader
// puts them in the same table anyway. So the union lives here, once, and both instruments read it.
//
// ⚠ SOURCES IS NOT DECORATION. A lane that says only "38 pairs" cannot be checked against the
// item that asked for it. Naming the corpus functions unioned, in order, is what lets a report
// state its own provenance and a guard assert it.

// Traffic names a population whose recall behaviour differs in kind, not degree: engineering
// prompts name versions, commands and technologies the entity gate can verify; consumer prompts
// mostly name nothing it can.
const (
	TrafficEngineering = "ENGINEERING"
	TrafficConsumer    = "CONSUMER"
)

// Kind names which side of the trade a lane measures.
const (
	KindRephrase = "rephrase" // SHOULD serve — these are recall
	KindDanger   = "danger"   // MUST NOT serve — these are precision
)

// Lane is one traffic population on one side of the trade.
type Lane struct {
	Traffic string
	Kind    string
	Pairs   []RephrasePair
	Sources []string
}

// Name is the lane's label in a report: "ENGINEERING rephrase".
func (l Lane) Name() string { return l.Traffic + " " + l.Kind }

// Lanes returns the four populations every hit-rate instrument in this repo measures, in the order
// cmd/hitrate has always printed them.
func Lanes() []Lane {
	return []Lane{
		{
			Traffic: TrafficEngineering, Kind: KindRephrase,
			Pairs:   union(EngineeringRephrasePairs()),
			Sources: []string{"EngineeringRephrasePairs"},
		},
		{
			Traffic: TrafficEngineering, Kind: KindDanger,
			Pairs:   union(EngineeringDangerPairs(), HeldOutDangerPairs()),
			Sources: []string{"EngineeringDangerPairs", "HeldOutDangerPairs"},
		},
		{
			Traffic: TrafficConsumer, Kind: KindRephrase,
			Pairs:   union(RephrasePairs(), ConsumerRephrasePairs()),
			Sources: []string{"RephrasePairs", "ConsumerRephrasePairs"},
		},
		{
			Traffic: TrafficConsumer, Kind: KindDanger,
			Pairs:   union(ConsumerDangerPairs(), ConsumerUnrelatedPairs()),
			Sources: []string{"ConsumerDangerPairs", "ConsumerUnrelatedPairs"},
		},
	}
}

// union concatenates corpora into a fresh slice. It copies deliberately: `append(a, b...)` on a
// package corpus can write into that corpus's backing array, so two instruments building lanes in
// different orders could observe different pairs.
func union(corpora ...[]RephrasePair) []RephrasePair {
	n := 0
	for _, c := range corpora {
		n += len(c)
	}
	out := make([]RephrasePair, 0, n)
	for _, c := range corpora {
		out = append(out, c...)
	}
	return out
}

// ByTraffic groups Lanes() into the rephrase/danger pair every hit-rate instrument prints as one
// section. It exists so cmd/hitrate, cmd/lens canoncheck and cmd/lens d2qcheck stop each building
// that grouping inline: three inline copies is how #393 came to publish a doc2query verdict over a
// population that did not overlap the one W2.1, W2.5 and W2.6 reported.
type TrafficLanes struct {
	Traffic  string
	Rephrase []RephrasePair
	Danger   []RephrasePair
}

// ByTraffic returns one entry per traffic population, in the order cmd/hitrate has always printed.
func ByTraffic() []TrafficLanes {
	out := []TrafficLanes{{Traffic: TrafficEngineering}, {Traffic: TrafficConsumer}}
	for _, l := range Lanes() {
		for i := range out {
			if out[i].Traffic != l.Traffic {
				continue
			}
			if l.Kind == KindRephrase {
				out[i].Rephrase = l.Pairs
			} else {
				out[i].Danger = l.Pairs
			}
		}
	}
	return out
}
