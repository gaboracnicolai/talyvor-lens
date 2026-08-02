// Package realdist measures how similar REAL prompts are to each other, using only the vectors
// already stored in prompt_embeddings.
//
// ⚠ WHY THIS EXISTS. Every threshold and doc2query verdict so far rests on a 28-pair synthetic
// corpus that was written adversarially for the PRECISION work — the rephrasings deliberately share
// as little wording as possible. That is what made the entity-gate finding credible and it is
// exactly what makes the recall findings pessimistic. There is direct evidence the corpus is
// harder than reality: the live pair that started this work — "What is the capital of the United
// Kingdom?" / "Which city is the capital of the UK?" — scored 0.8681, ABOVE every one of the 28
// engineering pairs. One real rephrasing beat the entire synthetic set.
//
// ⚠ AND IT READS NO PROMPT TEXT, BECAUSE THERE IS NONE TO READ. prompt_embeddings stores
// prompt_hash and a vector, never the prompt. token_events has a prompt_text column but it is
// populated ONLY under the `full` logging policy; the default is `metadata`, which strips it. So
// the honest question is not "how do we get the prompts" but "what can the vectors alone settle" —
// and the answer is: the thing that actually decides the product.
//
// ⚠ THE PRIVACY COST IS ZERO, AND THAT IS A DESIGN CONSTRAINT NOT A CONVENIENCE. The policy page
// states prompts are not retained by default. This package reads embedding, provider, model,
// discriminators, is_poolable and timestamps. It never selects `response`, never selects
// `prompt_text`, and never reconstructs either. Nothing has to be turned on, no tester has to
// consent to anything, and the statement on the policy page stays true.
//
// WHAT IT MEASURES. For every stored prompt: how close is its NEAREST OTHER stored prompt? That
// distribution answers the product question directly — if real traffic routinely puts two prompts
// at 0.90+, the pool serves people; if the nearest neighbour is typically 0.6, it serves nobody,
// and no amount of recall engineering changes that. It is the real-traffic counterpart of the
// synthetic distribution, measured the same way.
package realdist

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Row is one prompt's nearest-neighbour result. There is deliberately no text field: the type
// cannot carry a prompt even if a future caller wanted it to.
type Row struct {
	Poolable bool
	// NearestSim is the similarity to the closest OTHER stored prompt of the same
	// provider/model/embedding-model.
	NearestSim float64
	// NearestGatedSim is the same, restricted to neighbours the entity gate would allow
	// (identical discriminators). Zero when no gate-allowed neighbour exists.
	NearestGatedSim float64
	// GateComparable is false when either side predates migration 0112 and has NULL
	// discriminators, so the gated figure is unknown rather than zero.
	GateComparable bool
}

// Querier is the read-only surface this package needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
}

// Rows mirrors pgx.Rows minimally so the package can be driven by a real pool or a fake.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

// nearestSQL finds, for each embedding row, its closest neighbour among comparable rows.
//
// ⚠ THE SELECT LIST IS THE PRIVACY BOUNDARY. It names embedding-derived numbers and nothing else.
// `response` and `prompt_text` appear nowhere in this package, and TestSQL_SelectsNoContentColumns
// asserts that rather than trusting review.
//
// Comparability mirrors the pooled read exactly (provider, model, embedding_model, is_poolable):
// vectors from different embedding models are not on the same scale, and private rows are
// workspace-ID-prefixed so their similarities are inflated relative to pooled ones. Mixing either
// would produce a number that describes no real serve path.
const nearestSQL = `
SELECT a.is_poolable,
       COALESCE((SELECT 1 - (a.embedding <=> b.embedding)
                 FROM prompt_embeddings b
                 WHERE b.id <> a.id
                   AND b.provider = a.provider
                   AND b.model = a.model
                   AND b.embedding IS NOT NULL
                   AND b.is_poolable = a.is_poolable
                   AND b.embedding_model IS NOT DISTINCT FROM a.embedding_model
                 ORDER BY a.embedding <=> b.embedding
                 LIMIT 1), 0) AS nearest,
       COALESCE((SELECT 1 - (a.embedding <=> b.embedding)
                 FROM prompt_embeddings b
                 WHERE b.id <> a.id
                   AND b.provider = a.provider
                   AND b.model = a.model
                   AND b.embedding IS NOT NULL
                   AND b.is_poolable = a.is_poolable
                   AND b.embedding_model IS NOT DISTINCT FROM a.embedding_model
                   AND b.discriminators = a.discriminators
                 ORDER BY a.embedding <=> b.embedding
                 LIMIT 1), 0) AS nearest_gated,
       (a.discriminators IS NOT NULL) AS gate_comparable
FROM prompt_embeddings a
WHERE a.embedding IS NOT NULL`

// Measure runs the read and returns one Row per stored prompt.
func Measure(ctx context.Context, q Querier) ([]Row, error) {
	rows, err := q.Query(ctx, nearestSQL)
	if err != nil {
		return nil, fmt.Errorf("realdist: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Poolable, &r.NearestSim, &r.NearestGatedSim, &r.GateComparable); err != nil {
			return nil, fmt.Errorf("realdist scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SQL exposes the query so a test can assert what it does and does not select.
func SQL() string { return nearestSQL }

// Report renders the real distribution beside the synthetic one.
//
// ⚠ THE COMPARISON IS THE DELIVERABLE. A real distribution on its own says little; the same
// distribution next to the corpus that produced every prior verdict says exactly how much that
// corpus overstated the difficulty, which is the number that was asked for.
func Report(rows []Row, syntheticSorted []float64, syntheticLabel string) string {
	var pooled, private []float64
	var gated []float64
	var comparable int
	for _, r := range rows {
		if r.Poolable {
			pooled = append(pooled, r.NearestSim)
		} else {
			private = append(private, r.NearestSim)
		}
		if r.GateComparable {
			comparable++
			gated = append(gated, r.NearestGatedSim)
		}
	}
	sort.Float64s(pooled)
	sort.Float64s(private)
	sort.Float64s(gated)

	var b strings.Builder
	fmt.Fprintf(&b, "REAL NEAREST-NEIGHBOUR DISTRIBUTION — %d stored prompts\n", len(rows))
	fmt.Fprintf(&b, "  pooled rows: %d   private rows: %d   gate-comparable (post-0112): %d\n\n", len(pooled), len(private), comparable)
	if len(rows) == 0 {
		fmt.Fprintf(&b, "  ⚠ NO ROWS. An empty prompt_embeddings cannot answer this question, and an\n"+
			"    empty result must not be read as 'real traffic is dissimilar'. Run this against a\n"+
			"    deployment that has served real traffic.\n")
		return b.String()
	}
	writeDist(&b, "  POOLED (raw prompt — what cross-tenant serving actually compares)", pooled)
	writeDist(&b, "  PRIVATE (workspace-ID prefixed — inflated, NOT comparable to pooled)", private)
	if len(gated) > 0 {
		writeDist(&b, "  POOLED+GATED (nearest neighbour the entity gate would allow)", gated)
	}

	fmt.Fprintf(&b, "\n  SIDE BY SIDE — real pooled vs %s\n", syntheticLabel)
	fmt.Fprintf(&b, "    percentile     real      synthetic     delta\n")
	for _, p := range []float64{0.50, 0.75, 0.90, 1.0} {
		r := pct(pooled, p)
		s := pct(syntheticSorted, p)
		fmt.Fprintf(&b, "    %-12s  %.4f    %.4f      %+.4f\n", label(p), r, s, r-s)
	}

	fmt.Fprintf(&b, "\n  PROMPTS WITH A NEIGHBOUR AT OR ABOVE EACH THRESHOLD (pooled)\n")
	for _, t := range []float64{0.95, 0.92, 0.90, 0.88, 0.85, 0.83, 0.80} {
		n := countAtLeast(pooled, t)
		var g string
		if len(gated) > 0 {
			g = fmt.Sprintf("   gated: %d (%.0f%%)", countAtLeast(gated, t), share(gated, t))
		}
		fmt.Fprintf(&b, "    %.2f   %4d / %d  (%.0f%%)%s\n", t, n, len(pooled), share(pooled, t), g)
	}
	return b.String()
}

func writeDist(b *strings.Builder, title string, xs []float64) {
	if len(xs) == 0 {
		fmt.Fprintf(b, "%s: none\n", title)
		return
	}
	fmt.Fprintf(b, "%s (n=%d)\n", title, len(xs))
	fmt.Fprintf(b, "    min %.4f · p10 %.4f · p25 %.4f · median %.4f · p75 %.4f · p90 %.4f · max %.4f\n",
		pct(xs, 0), pct(xs, .10), pct(xs, .25), pct(xs, .50), pct(xs, .75), pct(xs, .90), pct(xs, 1))
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	return xs[int(p*float64(len(xs)-1))]
}

func countAtLeast(xs []float64, t float64) int {
	var n int
	for _, x := range xs {
		if x >= t {
			n++
		}
	}
	return n
}

func share(xs []float64, t float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	return float64(countAtLeast(xs, t)) / float64(len(xs)) * 100
}

func label(p float64) string {
	switch p {
	case 0.50:
		return "median"
	case 0.75:
		return "p75"
	case 0.90:
		return "p90"
	default:
		return "max"
	}
}
