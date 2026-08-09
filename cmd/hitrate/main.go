// Command hitrate measures the pooled-cache HIT RATE and FALSE-SERVE RATE at every candidate
// threshold, for engineering and consumer traffic separately, under the PREDICATE PRODUCTION
// ACTUALLY USES.
//
// ⚠ WHY THIS EXISTS AND WHY IT IS NOT MeasureRephrase. poolsafety.MeasureRephrase answers
// "how many rephrasings clear the cosine threshold". That is ONE of the two conditions a pooled
// serve requires. semanticSelectPooledSQL filters `discriminators = $6` in SQL — an EXACT entity
// match — and only then ranks by vector distance, and only then does Go compare the winner to the
// threshold. A hit rate measured on similarity alone therefore reports a number the product
// cannot deliver, and it moves when the threshold moves, which invites tuning a knob that is not
// the binding one.
//
// So every figure here is reported TWICE:
//
//	sim-only    — similarity >= t. What a threshold-tuning conversation implicitly assumes.
//	production  — similarity >= t AND discriminator.Match(A, B). What the SQL does.
//
// The gap between those two columns is the finding. The entity gate is threshold-independent, so
// the production column has a CEILING that no threshold can raise: the fraction of pairs whose
// discriminators are equal at all. That ceiling is printed on its own line because it, not the
// threshold, is what bounds the product.
//
// It recommends nothing and changes nothing. Threshold selection is not a measurement.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/poolsafety"
)

// thresholds are the four W2.1 asks for. 0.98 is included even though no committed harness
// measured it before: a threshold nobody has measured cannot be argued about.
var thresholds = []float64{0.98, 0.95, 0.92, 0.88}

// lane is one traffic population: the rephrasings that SHOULD serve and the pairs that MUST NOT.
type lane struct {
	name     string
	rephrase []poolsafety.RephrasePair
	danger   []poolsafety.RephrasePair
}

// scored is one measured pair plus the entity-gate verdict. Both are recorded per pair because
// the interesting rows are the ones where they disagree.
type scored struct {
	name     string
	sim      float64
	entityOK bool
	canonA   string
	canonB   string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hitrate:", err)
		os.Exit(1)
	}
}

func run() error {
	key := os.Getenv("LENS_OPENAI_API_KEY")
	if key == "" {
		return fmt.Errorf("no LENS_OPENAI_API_KEY — cannot measure, and an unmeasured hit rate must not be reported as one")
	}
	model := os.Getenv("LENS_EMBEDDING_MODEL")
	if model == "" {
		model = "text-embedding-3-small"
	}
	emb := embedder.NewOpenAIEmbedder(key, model, os.Getenv("LENS_EMBEDDING_BASE_URL"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	lanes := []lane{
		{
			name:     "ENGINEERING",
			rephrase: poolsafety.EngineeringRephrasePairs(),
			danger:   append(append([]poolsafety.RephrasePair{}, poolsafety.EngineeringDangerPairs()...), poolsafety.HeldOutDangerPairs()...),
		},
		{
			name:     "CONSUMER",
			rephrase: append(append([]poolsafety.RephrasePair{}, poolsafety.RephrasePairs()...), poolsafety.ConsumerRephrasePairs()...),
			danger:   append(append([]poolsafety.RephrasePair{}, poolsafety.ConsumerDangerPairs()...), poolsafety.ConsumerUnrelatedPairs()...),
		},
	}

	fmt.Printf("embedding model: %s\n", model)
	fmt.Printf("predicate:       similarity >= t  AND  discriminators equal (semanticSelectPooledSQL)\n\n")

	for _, ln := range lanes {
		reph, err := score(ctx, emb, ln.rephrase)
		if err != nil {
			return err
		}
		dang, err := score(ctx, emb, ln.danger)
		if err != nil {
			return err
		}
		report(ln.name, reph, dang)
	}
	return nil
}

// score embeds both sides of every pair, cosines them, and independently asks the entity gate
// whether the two prompts name the same things. The two verdicts are computed from the same
// prompt strings the SQL would see.
func score(ctx context.Context, emb poolsafety.Embedder, pairs []poolsafety.RephrasePair) ([]scored, error) {
	ps, err := poolsafety.ScorePairs(ctx, emb, pairs)
	if err != nil {
		return nil, err
	}
	out := make([]scored, 0, len(ps))
	for _, p := range ps {
		out = append(out, scored{
			name:     p.Pair.Name,
			sim:      p.Similarity,
			entityOK: discriminator.Match(p.Pair.A, p.Pair.B),
			canonA:   string(discriminator.Canon(p.Pair.A)),
			canonB:   string(discriminator.Canon(p.Pair.B)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sim > out[j].sim })
	return out, nil
}

func report(lane string, reph, dang []scored) {
	fmt.Printf("═══ %s ═══  %d rephrase pairs (should serve) · %d danger pairs (must not)\n",
		lane, len(reph), len(dang))

	fmt.Printf("\n  %-7s │ %-21s │ %-21s\n", "", "HIT RATE (rephrase)", "FALSE-SERVE (danger)")
	fmt.Printf("  %-7s │ %-10s %-10s │ %-10s %-10s\n", "thresh", "sim-only", "production", "sim-only", "production")
	fmt.Printf("  ────────┼──────────────────────┼──────────────────────\n")
	for _, t := range thresholds {
		hs, hp := count(reph, t)
		ds, dp := count(dang, t)
		fmt.Printf("  %-7.2f │ %-10s %-10s │ %-10s %-10s\n", t,
			frac(hs, len(reph)), frac(hp, len(reph)), frac(ds, len(dang)), frac(dp, len(dang)))
	}

	// The threshold-independent bound. Printed separately because it is the number that decides
	// the product: no threshold, however low, lifts the production column above it.
	ceilR := entityCeiling(reph)
	ceilD := entityCeiling(dang)
	fmt.Printf("\n  ENTITY-GATE CEILING (threshold-independent):\n")
	fmt.Printf("    rephrase pairs with equal discriminators: %s  ← production hit rate can never exceed this\n", frac(ceilR, len(reph)))
	fmt.Printf("    danger  pairs with equal discriminators: %s  ← what the gate does NOT catch\n", frac(ceilD, len(dang)))

	// ⚠ VACUITY CONTROL ON THE GATE ITSELF. `discriminators = $6` compares an EXTRACTED token set,
	// and the extractor can return NOTHING. Two prompts that each yield an empty set compare
	// EQUAL ('' = ''), so the gate returns TRUE having verified nothing. That is a pass, not a
	// match, and on a population where it is the common case the gate is not a safety mechanism at
	// all — it is a pass-through wearing one's name. Split the "equal" count so a reader can never
	// mistake one for the other.
	fmt.Printf("\n  ⚠ OF WHICH, EQUAL-BECAUSE-EMPTY (the gate extracted no entity from either side):\n")
	fmt.Printf("    rephrase: %s of all pairs   danger: %s of all pairs\n",
		frac(bothEmpty(reph), len(reph)), frac(bothEmpty(dang), len(dang)))
	fmt.Printf("    → as a share of the pairs the gate PASSED: rephrase %s · danger %s\n",
		frac(bothEmpty(reph), ceilR), frac(bothEmpty(dang), ceilD))

	// The pairs the gate refuses despite clearing every threshold are where hits are actually
	// being lost — naming them is what makes the ceiling actionable rather than a bare number.
	fmt.Printf("\n  REPHRASINGS THE ENTITY GATE REFUSES (sim >= 0.88, discriminators differ):\n")
	n := 0
	for _, s := range reph {
		if s.sim >= 0.88 && !s.entityOK {
			fmt.Printf("    %-22s %.4f   A=%q  B=%q\n", s.name, s.sim, s.canonA, s.canonB)
			n++
		}
	}
	if n == 0 {
		fmt.Printf("    (none — at 0.88 the threshold is refusing them before the gate ever runs)\n")
	}

	fmt.Printf("\n  TOP 5 REPHRASINGS BY SIMILARITY:\n")
	for i, s := range reph {
		if i >= 5 {
			break
		}
		fmt.Printf("    %-22s %.4f  entity=%-5v\n", s.name, s.sim, s.entityOK)
	}
	fmt.Printf("\n  TOP 5 DANGER PAIRS BY SIMILARITY:\n")
	for i, s := range dang {
		if i >= 5 {
			break
		}
		fmt.Printf("    %-22s %.4f  entity=%-5v\n", s.name, s.sim, s.entityOK)
	}
	fmt.Println()
}

func count(ss []scored, t float64) (simOnly, production int) {
	for _, s := range ss {
		if s.sim >= t {
			simOnly++
			if s.entityOK {
				production++
			}
		}
	}
	return
}

// bothEmpty counts pairs where the extractor produced no discriminator on EITHER side, so the
// SQL equality is ” = ” — true by absence of evidence rather than by agreement.
func bothEmpty(ss []scored) int {
	n := 0
	for _, s := range ss {
		if s.canonA == "" && s.canonB == "" {
			n++
		}
	}
	return n
}

func entityCeiling(ss []scored) int {
	n := 0
	for _, s := range ss {
		if s.entityOK {
			n++
		}
	}
	return n
}

func frac(n, d int) string {
	if d == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d %.0f%%", n, d, 100*float64(n)/float64(d))
}
