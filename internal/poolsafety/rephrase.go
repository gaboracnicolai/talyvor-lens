package poolsafety

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// REPHRASE HIT-RATE MEASUREMENT — the number the consumer product rests on.
//
// poolsafety.Check answers "can two UNRELATED prompts collide?" — the safety question, and it
// measures a CEILING. This answers the opposite one: "do two people asking the SAME question in
// DIFFERENT WORDS actually match?" — the utility question, and it measures a FLOOR.
//
// The two are the same measurement from opposite ends, and the threshold sits between them. A
// deployment needs both numbers: the gap between the unrelated ceiling and the rephrase floor is
// the whole operating room, and it is easy to know one and assume the other.
//
// ⚠ WHY THIS EXISTS AS A COMMITTED HARNESS. The pool's promise to a consumer is "if someone
// already asked it, you get their answer". That promise is a claim about REPHRASINGS, not about
// exact repeats — exact repeats hit the exact-key cache and never reach this path. Nobody had
// measured it, and an unmeasured claim on a customer-facing surface is the same class of thing as
// an unmeasured safety margin.
//
// ⚠ IT REPORTS A DISTRIBUTION, NOT A MEAN. An average hides the shape, and the shape is the
// finding: "most pairs at 0.95 with a long tail below 0.80" and "everything clustered at 0.88"
// have similar means and completely different consequences for whether the pool serves anyone.

// RephrasePair is one question asked two ways, as a member of the public would type it. Short,
// unpunctuated variety, no shared scaffolding — a pair that shares six words is measuring the
// words rather than the meaning.
type RephrasePair struct {
	Name string
	A    string
	B    string
}

// RephrasePairs is the corpus. Chosen for CONSUMER-STYLE traffic: general knowledge, how-to,
// definitions, and everyday advice — the questions a public trial would actually receive.
//
// Deliberately NOT engineered to score well. Several pairs use a different sentence FORM for the
// same intent (question vs imperative), different register (formal vs casual), and different
// vocabulary for the key noun, because that is what real rephrasing looks like. A corpus of
// near-identical strings would report a hit rate the product does not have.
func RephrasePairs() []RephrasePair {
	return []RephrasePair{
		{"capital-uk", "What is the capital of the United Kingdom?", "Which city is the capital of the UK?"},
		{"capital-france", "What's the capital of France?", "Name the French capital city"},
		{"boil-egg", "How long should I boil an egg?", "Cooking time for a hard boiled egg?"},
		{"tie-tie", "How do I tie a tie?", "Steps for tying a necktie"},
		{"photosynthesis", "What is photosynthesis?", "Explain how plants make food from sunlight"},
		{"reset-router", "How do I reset my wifi router?", "My router needs restarting, what do I do"},
		{"cat-chocolate", "Can cats eat chocolate?", "Is chocolate poisonous to cats?"},
		{"tip-usa", "How much should I tip in America?", "What's the normal tipping amount in the US?"},
		{"remove-stain", "How do I get a red wine stain out of a carpet?", "Best way to remove wine from carpet"},
		{"jet-lag", "How do I get over jet lag?", "Tips for recovering from jetlag after a long flight"},
		{"password-strong", "What makes a strong password?", "How should I choose a secure password?"},
		{"tax-deadline", "When is the tax return deadline in the UK?", "Last date to file a self assessment tax return"},
		{"protein-daily", "How much protein do I need a day?", "Recommended daily protein intake for an adult"},
		{"car-battery", "Why won't my car start?", "Car makes a clicking noise and won't turn over"},
		{"vitamin-d", "Should I take vitamin D in winter?", "Is a vitamin D supplement worth it during winter months?"},
		{"sourdough", "How do I make sourdough starter?", "Steps to create a sourdough culture from scratch"},
		{"visa-japan", "Do I need a visa for Japan?", "Visa requirements for a British tourist visiting Japan"},
		{"sleep-hours", "How many hours of sleep do I need?", "Recommended amount of sleep for adults"},
		{"compound-interest", "What is compound interest?", "Explain how interest compounds over time"},
		{"plant-water", "How often should I water a houseplant?", "Watering schedule for indoor plants"},
		{"laptop-slow", "Why is my laptop so slow?", "My computer has got really sluggish, how do I speed it up"},
		{"marathon-train", "How do I train for a marathon?", "Training plan for running 26 miles"},
		{"resign-notice", "How much notice do I give when resigning?", "Notice period required when quitting a job"},
		{"dog-walk", "How long should I walk my dog?", "Daily exercise needed for a medium sized dog"},
		{"pension-start", "When should I start a pension?", "What age is best to begin saving for retirement?"},
		{"fix-leak", "How do I fix a dripping tap?", "Leaky faucet repair steps"},
		{"learn-guitar", "How long does it take to learn guitar?", "Time needed to get decent at playing guitar"},
		{"covid-symptoms", "What are the symptoms of covid?", "How do I know if I have coronavirus?"},
	}
}

// PairScore is one measured pair.
type PairScore struct {
	Pair       RephrasePair
	Similarity float64
	Hit        bool // at or above the threshold under measurement
}

// RephraseReport is the measurement. Every field is a number someone can act on; nothing is
// summarised away.
type RephraseReport struct {
	Threshold float64
	Model     string
	Scores    []PairScore // sorted ascending — the tail is the interesting end
	Hits      int
	// UnrelatedCeiling is the highest similarity among UNRELATED pairs, i.e. how far the
	// threshold could fall before a false cross-tenant match becomes possible on this corpus.
	UnrelatedCeiling      float64
	UnrelatedCeilingPair  string
	UnrelatedPairsChecked int
}

// HitRate is the fraction of rephrasings that would be served from the pool.
func (r RephraseReport) HitRate() float64 {
	if len(r.Scores) == 0 {
		return 0
	}
	return float64(r.Hits) / float64(len(r.Scores))
}

// Percentile returns the similarity at p (0..1) over the sorted scores — the distribution the
// caller was told to report instead of an average.
func (r RephraseReport) Percentile(p float64) float64 {
	if len(r.Scores) == 0 {
		return 0
	}
	i := int(p * float64(len(r.Scores)-1))
	return r.Scores[i].Similarity
}

// HitRateAt reports what the hit rate WOULD be at another threshold, without changing anything.
// Provided so a trade-off can be evaluated on evidence; this package never picks a threshold.
func (r RephraseReport) HitRateAt(threshold float64) float64 {
	if len(r.Scores) == 0 {
		return 0
	}
	n := 0
	for _, s := range r.Scores {
		if s.Similarity >= threshold {
			n++
		}
	}
	return float64(n) / float64(len(r.Scores))
}

// MeasureRephrase scores every pair through the LIVE embedder — the same embedder and the same
// cosine the serve path uses, so the numbers are the ones production would produce.
//
// ⚠ IT EMBEDS THE RAW PROMPT, because that is what the POOLED read embeds
// (proxy.trySemanticPooled → SemanticCache.GetPooled). The PRIVATE read embeds
// "<workspaceID>:<prompt>", so its scores are not comparable and must not be mixed in here.
func MeasureRephrase(ctx context.Context, emb Embedder, model string, threshold float64) (RephraseReport, error) {
	rep := RephraseReport{Threshold: threshold, Model: model}
	for _, p := range RephrasePairs() {
		va, err := emb.Embed(ctx, p.A)
		if err != nil {
			return rep, fmt.Errorf("poolsafety: embed %q: %w", p.Name, err)
		}
		vb, err := emb.Embed(ctx, p.B)
		if err != nil {
			return rep, fmt.Errorf("poolsafety: embed %q: %w", p.Name, err)
		}
		sim, err := cosine(va, vb)
		if err != nil {
			return rep, fmt.Errorf("poolsafety: cosine %q: %w", p.Name, err)
		}
		hit := sim >= threshold
		if hit {
			rep.Hits++
		}
		rep.Scores = append(rep.Scores, PairScore{Pair: p, Similarity: sim, Hit: hit})
	}
	sort.Slice(rep.Scores, func(i, j int) bool { return rep.Scores[i].Similarity < rep.Scores[j].Similarity })
	return rep, nil
}

// String renders the distribution. Deliberately verbose about the tail: the pairs BELOW the
// threshold are the ones that decide whether the product's promise holds.
func (r RephraseReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "REPHRASE HIT RATE — %d pairs, model %s, threshold %.2f\n\n", len(r.Scores), r.Model, r.Threshold)
	fmt.Fprintf(&b, "  hit rate: %d/%d = %.0f%%\n\n", r.Hits, len(r.Scores), r.HitRate()*100)
	fmt.Fprintf(&b, "  distribution (ascending — the tail is the point):\n")
	fmt.Fprintf(&b, "    min    %.4f\n    p10    %.4f\n    p25    %.4f\n    median %.4f\n    p75    %.4f\n    p90    %.4f\n    max    %.4f\n\n",
		r.Percentile(0), r.Percentile(0.10), r.Percentile(0.25), r.Percentile(0.50),
		r.Percentile(0.75), r.Percentile(0.90), r.Percentile(1))
	fmt.Fprintf(&b, "  every pair:\n")
	for _, s := range r.Scores {
		mark := "MISS"
		if s.Hit {
			mark = "hit "
		}
		fmt.Fprintf(&b, "    %s %.4f  %-18s %q / %q\n", mark, s.Similarity, s.Pair.Name, s.Pair.A, s.Pair.B)
	}
	if r.UnrelatedPairsChecked > 0 {
		fmt.Fprintf(&b, "\n  UNRELATED CEILING (how far the threshold could fall before a false match):\n")
		fmt.Fprintf(&b, "    worst unrelated pair: %.4f  (%s), over %d pairs\n",
			r.UnrelatedCeiling, r.UnrelatedCeilingPair, r.UnrelatedPairsChecked)
		fmt.Fprintf(&b, "    headroom below the current threshold: %.4f\n", r.Threshold-r.UnrelatedCeiling)
	}
	fmt.Fprintf(&b, "\n  hit rate at other thresholds (evidence only — this tool never picks one):\n")
	for _, t := range []float64{0.95, 0.92, 0.90, 0.88, 0.85, 0.80, 0.75} {
		fmt.Fprintf(&b, "    %.2f → %.0f%%\n", t, r.HitRateAt(t)*100)
	}
	return b.String()
}

// PREFIX LIFT — why the same rephrasing can hit privately and miss the pool.
//
// The two semantic reads share a threshold and share an embedder, so it is natural to assume they
// are the same measurement over different rows. They are not: the PRIVATE read embeds
// "<workspaceID>:<prompt>" and the POOLED read embeds the raw prompt. The workspace id is common
// to BOTH sides of every private comparison, so it is shared text that no rephrasing can vary —
// and shared text raises cosine similarity.
//
// A private hit is therefore NOT evidence that two questions are ≥threshold alike. This measures
// the difference rather than asserting it.

// PrefixLift is one pair scored both ways.
type PrefixLift struct {
	Pair       RephrasePair
	Raw        float64 // what the POOLED read compares
	Prefixed   float64 // what the PRIVATE read compares
	Lift       float64 // Prefixed - Raw
	OnlyPrefix bool    // misses raw, hits prefixed — the reported symptom, exactly
}

// PrefixLiftReport is the measurement.
type PrefixLiftReport struct {
	Threshold   float64
	WorkspaceID string
	Lifts       []PrefixLift
	Raised      int // pairs where the prefix lifted the score at all
	OnlyPrefix  int // pairs that MISS pooled but HIT private
	MeanLift    float64
}

// MeasurePrefixLift scores every pair raw and workspace-prefixed. workspaceID should be a
// realistic id — length and character mix change how much text is shared, so a toy value like
// "ws1" would understate the effect a production id has.
func MeasurePrefixLift(ctx context.Context, emb Embedder, workspaceID string, threshold float64) (PrefixLiftReport, error) {
	rep := PrefixLiftReport{Threshold: threshold, WorkspaceID: workspaceID}
	var sum float64
	for _, p := range RephrasePairs() {
		var v [4][]float32
		for i, text := range []string{p.A, p.B, workspaceID + ":" + p.A, workspaceID + ":" + p.B} {
			e, err := emb.Embed(ctx, text)
			if err != nil {
				return rep, fmt.Errorf("poolsafety: prefix-lift embed %q: %w", p.Name, err)
			}
			v[i] = e
		}
		raw, err := cosine(v[0], v[1])
		if err != nil {
			return rep, fmt.Errorf("poolsafety: prefix-lift cosine %q: %w", p.Name, err)
		}
		pre, err := cosine(v[2], v[3])
		if err != nil {
			return rep, fmt.Errorf("poolsafety: prefix-lift cosine %q: %w", p.Name, err)
		}
		l := PrefixLift{Pair: p, Raw: raw, Prefixed: pre, Lift: pre - raw, OnlyPrefix: raw < threshold && pre >= threshold}
		if l.Lift > 0 {
			rep.Raised++
		}
		if l.OnlyPrefix {
			rep.OnlyPrefix++
		}
		sum += l.Lift
		rep.Lifts = append(rep.Lifts, l)
	}
	if n := len(rep.Lifts); n > 0 {
		rep.MeanLift = sum / float64(n)
	}
	sort.Slice(rep.Lifts, func(i, j int) bool { return rep.Lifts[i].Lift > rep.Lifts[j].Lift })
	return rep, nil
}

func (r PrefixLiftReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nPREFIX LIFT — private embeds %q+prompt, pooled embeds the raw prompt (same threshold %.2f, same embedder)\n\n",
		r.WorkspaceID+":", r.Threshold)
	fmt.Fprintf(&b, "  mean lift: %+.4f over %d pairs\n", r.MeanLift, len(r.Lifts))
	fmt.Fprintf(&b, "  pairs the prefix RAISED: %d/%d\n", r.Raised, len(r.Lifts))
	fmt.Fprintf(&b, "  pairs that MISS pooled but HIT private: %d  ← the reported symptom\n\n", r.OnlyPrefix)
	fmt.Fprintf(&b, "  per pair (by lift, descending):\n")
	for _, l := range r.Lifts {
		mark := "  "
		if l.OnlyPrefix {
			mark = "⚠ "
		}
		fmt.Fprintf(&b, "    %s%-18s raw %.4f → prefixed %.4f  (%+.4f)\n", mark, l.Pair.Name, l.Raw, l.Prefixed, l.Lift)
	}
	return b.String()
}

// THE CONSUMER-SIDE UNRELATED CEILING — and why Corpus() does not supply it.
//
// Corpus() measures a specific hazard: two customers' CODING-AGENT prompts sharing a large system
// preamble, where the shared preamble is most of the text and the differing part is a diff. That
// was the right corpus for the traffic Lens had. It is four pairs, all developer traffic.
//
// ⚠ CONSUMER TRAFFIC FAILS DIFFERENTLY. The dangerous pair is not two unrelated essays; it is two
// questions that are nearly the SAME SENTENCE with one word changed, where the correct answer is
// completely different — chocolate is fatal to dogs and merely bad for cats; the tax deadline
// differs by country. High textual similarity plus a different correct answer is precisely the
// shape that makes a false pooled hit HARMFUL rather than merely wasteful, and it is the shape a
// preamble-dominated corpus cannot produce.
//
// Reporting Corpus()'s ceiling as "the" headroom for a consumer product would overstate the room
// available, because it was never measured against traffic of this shape.

// ConsumerUnrelatedPairs are questions that must NEVER pool: minimal edit distance, maximal
// difference in the correct answer. Serving A's answer to B here is a wrong answer, not a stale one.
func ConsumerUnrelatedPairs() []RephrasePair {
	return []RephrasePair{
		{"chocolate-species", "Can cats eat chocolate?", "Can dogs eat chocolate?"},
		{"capital-country", "What is the capital of the United Kingdom?", "What is the capital of Australia?"},
		{"visa-country", "Do I need a visa for Japan?", "Do I need a visa for Brazil?"},
		{"tax-country", "When is the tax return deadline in the UK?", "When is the tax return deadline in the US?"},
		{"egg-method", "How long should I boil an egg?", "How long should I fry an egg?"},
		{"intake-substance", "How much protein do I need a day?", "How much water do I need a day?"},
		{"vitamin-which", "Should I take vitamin D in winter?", "Should I take vitamin C in winter?"},
		{"dose-who", "How much paracetamol can I take?", "How much paracetamol can a child take?"},
		{"drive-side", "Which side of the road do they drive on in Japan?", "Which side of the road do they drive on in France?"},
		{"notice-direction", "How much notice do I give when resigning?", "How much notice must my employer give me?"},
	}
}

// PositiveControls prove the instrument BEFORE its numbers are believed. A hit rate of zero is the
// same shape a broken embedder, a wrong cosine, or a silently-failing HTTP client produces, and
// "the pool serves nobody" is too consequential a conclusion to draw from an unverified ruler.
//
// Identical text MUST score ~1.0. A one-character typo MUST score near 1.0. If either fails, every
// other number in this report is meaningless and the run must be treated as void.
func PositiveControls() []RephrasePair {
	return []RephrasePair{
		{"identical", "What is the capital of the United Kingdom?", "What is the capital of the United Kingdom?"},
		{"typo", "What is the capital of the United Kingdom?", "What is the capitol of the United Kingdom?"},
		{"whitespace", "What is the capital of the United Kingdom?", "What is the capital of the United Kingdom? "},
	}
}

// ScorePairs is the shared measurement primitive: embed both sides, cosine, sorted descending.
func ScorePairs(ctx context.Context, emb Embedder, pairs []RephrasePair) ([]PairScore, error) {
	out := make([]PairScore, 0, len(pairs))
	for _, p := range pairs {
		va, err := emb.Embed(ctx, p.A)
		if err != nil {
			return nil, fmt.Errorf("poolsafety: embed %q: %w", p.Name, err)
		}
		vb, err := emb.Embed(ctx, p.B)
		if err != nil {
			return nil, fmt.Errorf("poolsafety: embed %q: %w", p.Name, err)
		}
		sim, err := cosine(va, vb)
		if err != nil {
			return nil, fmt.Errorf("poolsafety: cosine %q: %w", p.Name, err)
		}
		out = append(out, PairScore{Pair: p, Similarity: sim})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	return out, nil
}

// SEPARATION — the table that makes the trade-off decidable.
//
// A hit rate alone invites "then lower the threshold". That is only a valid move if the
// rephrasings sit ABOVE the dangerous pairs, i.e. if the two populations SEPARATE. If they
// overlap, lowering the threshold buys hits and false matches together, and if they INVERT — the
// best rephrasing scoring below the worst dangerous pair — then no threshold exists that serves
// any rephrasing at all without first admitting a wrong answer.
//
// This prints both counts at every candidate threshold so the trade is visible rather than argued.
// It recommends nothing.
func SeparationTable(rephrase, dangerous []PairScore) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nSEPARATION — at each candidate threshold: rephrasings SERVED vs wrong answers ADMITTED\n\n")
	fmt.Fprintf(&b, "    threshold   served/%d   admitted/%d\n", len(rephrase), len(dangerous))
	for _, t := range []float64{0.95, 0.92, 0.90, 0.89, 0.88, 0.87, 0.86, 0.85, 0.83, 0.80, 0.75, 0.70} {
		var served, admitted int
		for _, s := range rephrase {
			if s.Similarity >= t {
				served++
			}
		}
		for _, d := range dangerous {
			if d.Similarity >= t {
				admitted++
			}
		}
		note := ""
		if admitted > 0 && served == 0 {
			note = "  ← admits wrong answers while serving NOBODY"
		} else if admitted > 0 {
			note = "  ← both"
		}
		fmt.Fprintf(&b, "    %.2f        %2d         %2d%s\n", t, served, admitted, note)
	}

	var bestRe, worstDanger float64
	for _, s := range rephrase {
		if s.Similarity > bestRe {
			bestRe = s.Similarity
		}
	}
	worstDanger = 1
	for _, d := range dangerous {
		if d.Similarity < worstDanger {
			worstDanger = d.Similarity
		}
	}
	var maxDanger float64
	for _, d := range dangerous {
		if d.Similarity > maxDanger {
			maxDanger = d.Similarity
		}
	}
	fmt.Fprintf(&b, "\n  best genuine rephrasing:      %.4f\n", bestRe)
	fmt.Fprintf(&b, "  worst dangerous unrelated:    %.4f\n", maxDanger)
	if maxDanger > bestRe {
		fmt.Fprintf(&b, "\n  ⚠⚠ INVERTED: the highest-scoring genuine rephrasing (%.4f) scores BELOW the most\n", bestRe)
		fmt.Fprintf(&b, "     dangerous unrelated pair (%.4f). NO threshold serves even one rephrasing without\n", maxDanger)
		fmt.Fprintf(&b, "     first admitting a pair whose correct answer is different. This is not a threshold\n")
		fmt.Fprintf(&b, "     that is set too high; it is an embedding that does not separate these populations.\n")
	}
	return b.String()
}

// CosineOf exposes the package cosine so measurement commands compute similarity with the SAME
// function the serve path uses, rather than a second implementation that could drift from it.
func CosineOf(a, b []float32) (float64, error) { return cosine(a, b) }
