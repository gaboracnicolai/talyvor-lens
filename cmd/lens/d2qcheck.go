package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/doc2query"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/poolsafety"
)

// `lens d2qcheck` — does deriving questions from stored ANSWERS widen recall, and at what cost to
// the danger ceiling?
//
// ⚠ IT SIMULATES THE REAL PIPELINE, not a proxy for it. For each pair (A,B): A is asked, a real
// answer is GENERATED for it, variants are derived FROM THAT ANSWER, and then B is scored against
// {A} ∪ variants — which is exactly the vector set the pooled read would search. Scoring B against
// the corpus questions directly would measure something doc2query does not do.
//
// ⚠ THE GATE IS APPLIED THROUGHOUT, on the original's discriminators, because that is what variant
// rows inherit. A pair the gate refuses cannot pool through ANY variant, so doc2query cannot widen
// the danger surface across an entity boundary — only inside one.
//
// ⚠ AND THAT LAST SENTENCE IS ALSO THE CEILING, WHICH #393 STATED AS A SAFETY PROPERTY AND NOT AS
// A LIMIT. `GetPooled` refuses an unverifiable prompt before it embeds, and `discriminators = $6`
// then requires exact entity equality — so for any pair where discriminator.Match(A,B) is false,
// doc2query's production recall is ZERO no matter how good the variants are, at every threshold.
// The gate-allowed count is therefore printed as its own line: it is the number that bounds this
// mechanism, and it is knowable without a single API call.
type d2qResult struct {
	Pair       poolsafety.RephrasePair
	Baseline   float64 // B vs A — what the pool could do before doc2query
	Best       float64 // B vs the best of {A} ∪ variants
	BestVia    string
	GateAllows bool
	NumVars    int

	// VarScores is this pair's B-vs-variant score, one per derived variant, in derivation order.
	//
	// ⚠ IT LIVES ON THE RESULT BECAUSE IT USED TO LIVE IN A PACKAGE-LEVEL MAP KEYED BY Pair.Name,
	// and a pair name is a label, not a key. poolsafety had one collision — "notice-direction"
	// named a landlord pair in ConsumerDangerPairs and an employment pair in
	// ConsumerUnrelatedPairs, both of which the CONSUMER danger lane unions — so the second corpus
	// measured silently overwrote the first and BestAtN reported one lane's variants under the
	// other lane's pair. That pair has since been renamed and poolsafety now guards uniqueness
	// within a lane, but carrying the scores with the result is what makes the class of defect
	// unrepresentable rather than merely absent today.
	VarScores []float64
}

// d2qCorpus is one corpus this harness measures.
type d2qCorpus struct {
	name    string
	traffic string
	kind    string
	pairs   []poolsafety.RephrasePair
	sources []string
}

// d2qCorpora is the population this harness measures.
//
// ⚠ IT READS poolsafety.Lanes() RATHER THAN NAMING CORPORA INLINE, and that is the whole point.
// It used to list three engineering corpora, so #393 published doc2query's verdict over 68 pairs
// of which none were consumer traffic — beside W2.1's, W2.5's and W2.6's figures, which were over
// all 154. Two instruments with inline populations cannot notice that they disagree about what
// they are measuring.
func d2qCorpora() []d2qCorpus {
	lanes := poolsafety.Lanes()
	out := make([]d2qCorpus, 0, len(lanes))
	for _, l := range lanes {
		out = append(out, d2qCorpus{
			name: l.Name(), traffic: l.Traffic, kind: l.Kind, pairs: l.Pairs, sources: l.Sources,
		})
	}
	return out
}

func runD2QCheck() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("d2qcheck: config: %w", err)
	}
	if cfg.OpenAIAPIKey == "" || cfg.AnthropicAPIKey == "" {
		return errors.New("d2qcheck: needs LENS_OPENAI_API_KEY (embeddings) and LENS_ANTHROPIC_API_KEY (deriving)")
	}
	meter := &costMeter{}
	emb := &meteredEmbedder{e: embedder.NewOpenAIEmbedder(cfg.OpenAIAPIKey, cfg.EmbeddingModel, cfg.EmbeddingBaseURL), m: meter}
	der := doc2query.NewAnthropicDeriver(cfg.AnthropicAPIKey, "claude-haiku-4-5")
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Minute)
	defer cancel()

	maxVars := 8
	corpora := d2qCorpora()

	fmt.Printf("doc2query measurement — embedder %s, deriver claude-haiku-4-5, up to %d variants\n", cfg.EmbeddingModel, maxVars)
	fmt.Printf("predicate: production = discriminators equal (semanticSelectPooledSQL) AND max cos(B, {A}∪variants) >= t\n")
	fmt.Printf("           sim-only   = the similarity condition ALONE, which no threshold move can make the product's\n\n")

	all := map[string][]d2qResult{}
	for _, c := range corpora {
		fmt.Fprintf(os.Stderr, "measuring %s (%d pairs, sources %v)…\n", c.name, len(c.pairs), c.sources)
		res, err := measureCorpus(ctx, emb, der, der, c.pairs, maxVars, meter)
		if err != nil {
			return fmt.Errorf("d2qcheck %s: %w", c.name, err)
		}
		all[c.name] = res
	}

	for _, c := range corpora {
		res := all[c.name]
		fmt.Printf("\n=== %s (%d pairs, %v) ===\n", c.name, len(res), c.sources)
		sort.Slice(res, func(i, j int) bool { return res[i].Best > res[j].Best })
		for _, r := range res {
			gate := "gate:REFUSED"
			if r.GateAllows {
				gate = "gate:allowed"
			}
			fmt.Printf("  base %.4f -> best %.4f (%+.4f) %-13s vars=%d %-22s via %q\n",
				r.Baseline, r.Best, r.Best-r.Baseline, gate, r.NumVars, r.Pair.Name, r.BestVia)
		}
	}

	for _, traffic := range []string{poolsafety.TrafficEngineering, poolsafety.TrafficConsumer} {
		reph := all[traffic+" "+poolsafety.KindRephrase]
		danger := all[traffic+" "+poolsafety.KindDanger]
		renderSweep(os.Stdout, traffic, reph, danger)
	}

	meter.report(os.Stdout, totalVariants(all), totalPairs(all))
	return nil
}

func totalVariants(all map[string][]d2qResult) int {
	n := 0
	for _, rs := range all {
		for _, r := range rs {
			n += r.NumVars
		}
	}
	return n
}

func totalPairs(all map[string][]d2qResult) int {
	n := 0
	for _, rs := range all {
		n += len(rs)
	}
	return n
}

// sweepNs and sweepThresholds are the sweep's two axes.
var (
	sweepNs         = []int{0, 1, 3, 5, 8}
	sweepThresholds = []float64{0.92, 0.85, 0.83}
)

// renderSweep prints the variant-count sweep for one traffic population.
//
// ⚠ EVERY DENOMINATOR IS len() OF THE SLICE THAT WAS COUNTED, never a literal. #393 published this
// table as "0/28 … 1/28" with the 28 typed into the format string; EngineeringRephrasePairs() has
// held 30 pairs since, so those figures are stated over a population two pairs smaller than the one
// they were measured on, and no code path could notice. A rate whose denominator is not derived
// from its own population is a claim about a corpus that may no longer exist.
//
// ⚠ THE GATE-ALLOWED LINE IS PRINTED FIRST BECAUSE IT IS THE BINDING NUMBER. The production
// columns cannot exceed it at any threshold or any variant count, and it costs no API call to
// know. A sweep read without it invites tuning N and t against a ceiling neither one touches.
func renderSweep(w io.Writer, traffic string, reph, danger []d2qResult) {
	fmt.Fprintf(w, "\n=== %s VARIANT-COUNT SWEEP ===  %d rephrase (should serve) · %d danger (must not)\n",
		traffic, len(reph), len(danger))
	fmt.Fprintf(w, "  gate-allowed: rephrase %d/%d · danger %d/%d   ← threshold- AND variant-independent; production cannot exceed it\n",
		gateAllowed(reph), len(reph), gateAllowed(danger), len(danger))
	fmt.Fprintf(w, "  N   thresh    HIT prod  HIT sim   │ FALSE prod FALSE sim\n")
	for _, n := range sweepNs {
		for _, th := range sweepThresholds {
			hp := countAt(reph, n, th, true)
			hs := countAt(reph, n, th, false)
			dp := countAt(danger, n, th, true)
			ds := countAt(danger, n, th, false)
			fmt.Fprintf(w, "  %d   @%.2f     %2d/%-5d %2d/%-5d │ %2d/%-6d %2d/%-5d\n",
				n, th, hp, len(reph), hs, len(reph), dp, len(danger), ds, len(danger))
		}
	}
}

func gateAllowed(rs []d2qResult) int {
	n := 0
	for _, r := range rs {
		if r.GateAllows {
			n++
		}
	}
	return n
}

func countAt(rs []d2qResult, n int, th float64, gated bool) int {
	var c int
	for _, r := range rs {
		if gated && !r.GateAllows {
			continue
		}
		s := r.Baseline
		if n > 0 && r.NumVars > 0 && r.Best > s {
			// Best is over all variants; approximate the first n by capping.
			if n >= r.NumVars {
				s = r.Best
			} else {
				s = r.BestAtN(n)
			}
		}
		if s >= th {
			c++
		}
	}
	return c
}

// BestAtN is the best match this pair reaches with only the FIRST n derived variants indexed —
// a real measurement at each sweep point rather than an interpolation between "none" and "all".
func (r d2qResult) BestAtN(n int) float64 {
	best := r.Baseline
	for i, s := range r.VarScores {
		if i >= n {
			break
		}
		if s > best {
			best = s
		}
	}
	return best
}

func measureCorpus(ctx context.Context, emb poolsafety.Embedder, der, gen doc2query.Deriver, pairs []poolsafety.RephrasePair, maxVars int, m *costMeter) ([]d2qResult, error) {
	out := make([]d2qResult, 0, len(pairs))
	for _, p := range pairs {
		vb, err := emb.Embed(ctx, p.B)
		if err != nil {
			return nil, err
		}
		va, err := emb.Embed(ctx, p.A)
		if err != nil {
			return nil, err
		}
		base, err := poolsafety.CosineOf(va, vb)
		if err != nil {
			return nil, err
		}

		// A real answer to A, then variants derived from THAT answer.
		answer, err := generateAnswer(ctx, gen, p.A, m)
		if err != nil {
			return nil, err
		}
		qs, err := deriveVariants(ctx, der, answer, maxVars, m)
		if err != nil {
			return nil, err
		}
		best, via := base, p.A
		scores := make([]float64, 0, len(qs))
		for _, q := range qs {
			vq, err := emb.Embed(ctx, q)
			if err != nil {
				return nil, err
			}
			s, err := poolsafety.CosineOf(vq, vb)
			if err != nil {
				return nil, err
			}
			scores = append(scores, s)
			if s > best {
				best, via = s, q
			}
		}
		out = append(out, d2qResult{
			Pair: p, Baseline: base, Best: best, BestVia: via,
			GateAllows: discriminator.Match(p.A, p.B), NumVars: len(qs), VarScores: scores,
		})
	}
	return out, nil
}

// generateAnswer produces the stored answer the pool would actually hold. Reuses the deriver's
// transport with a prompt that asks for an answer rather than questions.
//
// ⚠ ITS COST IS METERED SEPARATELY AND IS NOT DOC2QUERY'S. In production the answer already
// exists — it is the reply the user paid for — so charging it to this mechanism would overstate
// the write-time bill by roughly an order of magnitude. It is the harness's fixture cost.
func generateAnswer(ctx context.Context, gen doc2query.Deriver, question string, m *costMeter) (string, error) {
	ad, ok := gen.(*doc2query.AnthropicDeriver)
	if !ok {
		return question, nil
	}
	a, u, err := ad.AnswerWithUsage(ctx, question)
	if err != nil {
		return "", err
	}
	m.addAnswer(u)
	return a, nil
}

// deriveVariants is the call whose cost IS doc2query's, together with the variant embeddings.
func deriveVariants(ctx context.Context, der doc2query.Deriver, answer string, n int, m *costMeter) ([]string, error) {
	ad, ok := der.(*doc2query.AnthropicDeriver)
	if !ok {
		return der.Derive(ctx, answer, n)
	}
	qs, u, err := ad.DeriveWithUsage(ctx, answer, n)
	if err != nil {
		return nil, err
	}
	m.addDerive(u)
	return qs, nil
}

// ⚠ COST IS MEASURED, NOT ESTIMATED, AND THE TWO HALVES ARE KEPT APART.
//
// W2.7 asks for "cost per stored answer: N generations + N embeddings, at write time, measured not
// estimated", and notes that W2.6's "~50 tokens" estimate was 3.8x low. doc2query's own package
// doc carries an estimate too — "~800 input tokens of answer plus ~100 output tokens of questions"
// — and it is the number the break-even argument rests on. This meter reads the `usage` block the
// APIs already return, so the estimate can be checked rather than repeated.
//
// The split matters more than the total. Only the DERIVE call and the VARIANT embeddings are
// marginal cost that doc2query adds to a write; the answer generation and the A/B embeddings are
// the harness's own fixture, and folding them in would make the mechanism look ~10x dearer.
type costMeter struct {
	mu sync.Mutex

	answerCalls int
	answerUsage doc2query.Usage
	deriveCalls int
	deriveUsage doc2query.Usage
	embedCalls  int
	embedTokens int
}

// The add* methods are nil-safe so a test can drive measureCorpus without a meter.
func (m *costMeter) addAnswer(u doc2query.Usage) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answerCalls++
	m.answerUsage.Add(u)
}

func (m *costMeter) addDerive(u doc2query.Usage) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deriveCalls++
	m.deriveUsage.Add(u)
}

func (m *costMeter) addEmbed(tokens int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.embedCalls++
	m.embedTokens += tokens
}

func (m *costMeter) report(w io.Writer, variants, pairs int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintf(w, "\n=== MEASURED COST (from the APIs' own usage blocks, not estimated) ===\n")
	fmt.Fprintf(w, "  answers generated (HARNESS FIXTURE, not doc2query's cost):\n")
	fmt.Fprintf(w, "    %d calls · %d in · %d out\n", m.answerCalls, m.answerUsage.InTokens, m.answerUsage.OutTokens)
	fmt.Fprintf(w, "  derive calls (doc2query's marginal write-time cost):\n")
	fmt.Fprintf(w, "    %d calls · %d in · %d out\n", m.deriveCalls, m.deriveUsage.InTokens, m.deriveUsage.OutTokens)
	if m.deriveCalls > 0 {
		fmt.Fprintf(w, "    per stored answer: %.1f in · %.1f out tokens (package doc estimates ~800 in / ~100 out)\n",
			float64(m.deriveUsage.InTokens)/float64(m.deriveCalls), float64(m.deriveUsage.OutTokens)/float64(m.deriveCalls))
	}
	fmt.Fprintf(w, "  embeddings: %d calls · %d prompt tokens (ALL of them — corpus A/B plus variants)\n", m.embedCalls, m.embedTokens)
	if pairs > 0 {
		fmt.Fprintf(w, "    variants indexed: %d over %d pairs = %.2f per stored answer\n",
			variants, pairs, float64(variants)/float64(pairs))
	}
	if m.embedCalls > 0 {
		fmt.Fprintf(w, "    mean tokens per embedding call: %.1f\n", float64(m.embedTokens)/float64(m.embedCalls))
	}
}

// meteredEmbedder counts what the embeddings actually cost. It wraps rather than replaces so the
// harness embeds through exactly the client production uses.
type meteredEmbedder struct {
	e *embedder.OpenAIEmbedder
	m *costMeter
}

func (m *meteredEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	v, tokens, err := m.e.EmbedWithUsage(ctx, text)
	if err != nil {
		return nil, err
	}
	m.m.addEmbed(tokens)
	return v, nil
}
