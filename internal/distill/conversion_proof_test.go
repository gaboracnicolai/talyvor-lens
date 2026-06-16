package distill

// conversion_proof_test.go — DISTILL's savings-quality VALIDATION (U23).
//
// WHAT THIS PROVES (the bounded claim — read it before trusting a number):
//
//	Across the model-free converters (HTML/CSV/JSON/XML/text), the fidelity
//	tiers preserve ANSWER-RELEVANT INFORMATION, graded by a normalized
//	presence-check of curated reference facts against each tier's distilled
//	Markdown, PAIRED with the existing honest savings (TokensSaved as computed
//	by computeSavings — never recomputed rosier).
//
// WHAT THIS DOES *NOT* PROVE (stated, not buried):
//   - NOT "a model's answer is unchanged." We grade whether the facts a correct
//     answer would need survive the CONVERSION — a fact absent from the
//     distilled text cannot be answered by ANY model, so its loss is real and
//     model-independent. But preservation of the facts is necessary, not
//     sufficient, for an unchanged model answer. A live-model eval is a separate,
//     non-deterministic exercise out of scope here.
//   - NOT "conversion adds nothing wrong." Preservation is ONE-DIRECTIONAL: the
//     presence-check confirms facts did not VANISH; it does not detect spurious
//     or corrupted content the conversion might have introduced.
//   - NOT a quality judgment from ScoreResponse. ScoreResponse (quality/scorer.go)
//     is a shallow heuristic (length ratio, refusal phrases, truncation,
//     repetition, a markdown-structure bonus) that never compares prompt content
//     to response content — a well-formed WRONG answer scores ~1.0. It is a weak
//     judge and is deliberately NOT used here.
//
// THE OCR / DOCX / XLSX / PDF PATH IS UNPROVEN BY THIS HARNESS — AND IS THE
// HIGHER-RISK PATH. Vision-OCR is the lossiest conversion (a model reads pixels),
// exactly where savings-quality risk is highest. It is excluded here ONLY because
// it needs a vision model (non-deterministic, not CI-safe), NOT because it is
// low-risk. Its exclusion must not be read as coverage: it needs a separate
// model-based evaluation before any claim is made about it.
//
// No production surface: the harness lives entirely in this test file, reuses the
// package internals (Distill / applyTier / computeSavings), holds no ledger handle
// (mint-free by construction), reads no cross-tenant data, and needs no migration.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/talyvor/lens/internal/alerts"
)

// proofRefModel turns saved tokens into an ILLUSTRATIVE dollar figure via the
// real price table (alerts.CostUSD, input side). Named, not hidden — the $ column
// is "saved tokens priced at this model's input rate", nothing more.
const proofRefModel = "gpt-4o"

// factPreservationThreshold: a tier preserving fewer than this fraction of a
// fixture's answer-relevant facts is flagged DEGRADING. 1.0 = "no answer-relevant
// fact may vanish" — the strict, falsifiable bar.
const factPreservationThreshold = 1.0

// proofFixture is a synthetic document plus the curated answer-relevant facts a
// correct answer about it would need. Facts are placed in body prose / table /
// data cells so the outline tier's by-design loss is DETECTABLE. No production data.
type proofFixture struct {
	name   string
	format Format
	input  string
	facts  []string // verbatim, answer-relevant; must survive a non-degrading conversion
}

// proofCorpus — 7 fixtures across every model-free converter. Facts deliberately
// span heading (outline keeps) vs body/table/data (outline drops) so the proof
// quantifies the tradeoff rather than rubber-stamping it.
var proofCorpus = []proofFixture{
	{
		name:   "html_report",
		format: FormatHTML,
		input:  "<h1>Acme Logistics</h1><p>Quarterly revenue reached $4.2M this period.</p><table><tr><th>SKU</th><th>Units</th></tr><tr><td>WIDGET-7</td><td>1500</td></tr></table>",
		facts:  []string{"Acme Logistics", "$4.2M", "WIDGET-7"}, // heading / prose / table-cell
	},
	{
		// SPOTLIGHT (the degrading fixture): every fact is in body/table; the
		// heading carries none — so outline MUST drop all of them.
		name:   "html_contract",
		format: FormatHTML,
		input:  "<h2>Agreement Terms</h2><p>The penalty for late delivery is $50,000.</p><p>Governing law: Delaware.</p><table><tr><th>Milestone</th><th>Due</th></tr><tr><td>Final</td><td>2026-09-30</td></tr></table>",
		facts:  []string{"$50,000", "Delaware", "2026-09-30"},
	},
	{
		name:   "csv_inventory",
		format: FormatCSV,
		input:  "SKU,Name,OnHand,UnitPrice\nA100,Hex Bolt,4200,0.05\nB200,Lock Nut,3100,0.03\n",
		facts:  []string{"A100", "4200", "Lock Nut"}, // all in data cells
	},
	{
		name:   "json_config",
		format: FormatJSON,
		input:  `{"service":"auth-gateway","timeout_ms":3000,"region":"us-east-1"}`,
		facts:  []string{"auth-gateway", "3000", "us-east-1"}, // all in the code-fenced body
	},
	{
		name:   "xml_order",
		format: FormatXML,
		input:  `<order id="ORD-9914"><customer>Globex</customer><item sku="CBL-12">Cable</item><total>299.50</total></order>`,
		facts:  []string{"ORD-9914", "Globex", "299.50"},
	},
	{
		name:   "text_memo",
		format: FormatText,
		input:  "# Incident 4471 Summary\n\nRoot cause: a null dereference in the auth handler.\nImpact: 320 users saw elevated latency.\n",
		facts:  []string{"Incident 4471 Summary", "null dereference", "320 users"}, // heading / prose / prose
	},
	{
		name:   "html_catalog",
		format: FormatHTML,
		input:  "<h1>Plans</h1><h2>Enterprise</h2><p>The Enterprise tier is billed at $999 per seat annually.</p>",
		facts:  []string{"Enterprise", "$999"}, // heading / prose
	},
}

// tierResult pairs the HONEST savings (reused as-is) with the fact-preservation
// quality result for one tier. A saving is never reported without its quality.
type tierResult struct {
	tier           Tier
	tokensSaved    int
	savingsPct     float64
	costSavedUSD   float64
	factsPreserved int
	factsTotal     int
	degrading      bool
}

// normalizeForMatch lowercases and collapses whitespace runs, so a fact survives
// reformatting (e.g. a reflowed table cell) but a vanished fact is genuinely gone.
func normalizeForMatch(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

// factsPreservedIn counts how many reference facts are present in the distilled
// Markdown. This is THE quality signal — deterministic, model-independent.
func factsPreservedIn(markdown string, facts []string) int {
	hay := normalizeForMatch(markdown)
	n := 0
	for _, f := range facts {
		if strings.Contains(hay, normalizeForMatch(f)) {
			n++
		}
	}
	return n
}

// evalTiers converts the fixture once (faithful) and measures every tier off it,
// reusing computeSavings for the honest token delta and factsPreservedIn for the
// quality. Pure, offline, no model, no ledger, no cross-tenant read.
func evalTiers(t *testing.T, fx proofFixture) []tierResult {
	t.Helper()
	faithful, err := DistillAs(context.Background(), []byte(fx.input), fx.format)
	if err != nil {
		t.Fatalf("%s: DistillAs(%s): %v", fx.name, fx.format, err)
	}
	out := make([]tierResult, 0, 3)
	for _, tier := range []Tier{TierFaithful, TierStructured, TierOutline} {
		tiered := applyTier(faithful, tier) // faithful is unmutated (value semantics)
		sav := computeSavings([]byte(fx.input), tiered, false)
		pct := 0.0
		if sav.InputTokensRaw > 0 {
			pct = 100 * float64(sav.TokensSaved) / float64(sav.InputTokensRaw)
		}
		pres := factsPreservedIn(tiered.Markdown, fx.facts)
		out = append(out, tierResult{
			tier:           tier,
			tokensSaved:    sav.TokensSaved,
			savingsPct:     pct,
			costSavedUSD:   alerts.CostUSD(proofRefModel, sav.TokensSaved, 0),
			factsPreserved: pres,
			factsTotal:     len(fx.facts),
			degrading:      float64(pres) < factPreservationThreshold*float64(len(fx.facts)),
		})
	}
	return out
}

// spotlightFixture is html_contract — every fact in body/table, none in the
// heading — the fixture that proves the harness CAN detect degradation.
func spotlightFixture() proofFixture {
	for _, fx := range proofCorpus {
		if fx.name == "html_contract" {
			return fx
		}
	}
	panic("spotlight fixture missing")
}

// TestConversionProof_DetectsDegradation — THE SPINE. The spotlight fixture's
// answer-relevant facts all live in body/table that outline drops, so outline
// MUST be flagged degrading and preserve ZERO facts. A grader hard-wired to 100%
// (or any grader blind to fact loss) fails this test.
func TestConversionProof_DetectsDegradation(t *testing.T) {
	tiers := evalTiers(t, spotlightFixture())
	faithful, outline := tiers[0], tiers[2]
	if outline.tier != TierOutline {
		t.Fatalf("tier order changed: %q", outline.tier)
	}
	if !outline.degrading {
		t.Errorf("outline must be flagged DEGRADING for a body/table-fact document; preserved %d/%d",
			outline.factsPreserved, outline.factsTotal)
	}
	if outline.factsPreserved != 0 {
		t.Errorf("spotlight: outline should drop ALL %d body/table facts; preserved %d",
			outline.factsTotal, outline.factsPreserved)
	}
	// The harness must NOT cry wolf: faithful preserves every fact.
	if faithful.factsPreserved != faithful.factsTotal {
		t.Errorf("faithful must preserve all facts; got %d/%d", faithful.factsPreserved, faithful.factsTotal)
	}
}

// TestConversionProof_NoFalseDegradation — the lossless tiers (faithful,
// structured) preserve 100% of answer-relevant facts for EVERY fixture. If a
// "lossless" tier drops a fact, that's a real finding (converter or fixture),
// not something to paper over.
func TestConversionProof_NoFalseDegradation(t *testing.T) {
	for _, fx := range proofCorpus {
		for _, tr := range evalTiers(t, fx) {
			if tr.tier == TierOutline {
				continue // outline is permitted (and expected) to degrade
			}
			if tr.factsPreserved != tr.factsTotal {
				t.Errorf("%s/%s: a lossless tier dropped facts (%d/%d)",
					fx.name, tr.tier, tr.factsPreserved, tr.factsTotal)
			}
		}
	}
}

// TestConversionProof_Artifact — renders the per-fixture + aggregate
// savings×fact-preservation table (the proof artifact, captured in -v output) and
// pins the honest-accounting invariants: savings are non-negative, every saving
// is paired with a quality number, and the proof can come out NEGATIVE (at least
// one tier is flagged degrading — else it isn't a proof).
func TestConversionProof_Artifact(t *testing.T) {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "fixture\tformat\ttier\ttokens_saved\tsavings_%\t$saved@gpt-4o\tfacts\tverdict")

	anyDegrading := false
	// aggregate facts preserved per tier across the corpus
	aggPres := map[Tier][2]int{} // tier -> {preserved, total}
	for _, fx := range proofCorpus {
		for _, tr := range evalTiers(t, fx) {
			if tr.tokensSaved < 0 {
				t.Errorf("%s/%s: negative TokensSaved %d — honest savings must never go negative",
					fx.name, tr.tier, tr.tokensSaved)
			}
			verdict := "ok"
			if tr.degrading {
				verdict = "DEGRADING"
				anyDegrading = true
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%.1f\t%.6f\t%d/%d\t%s\n",
				fx.name, fx.format, tr.tier, tr.tokensSaved, tr.savingsPct, tr.costSavedUSD,
				tr.factsPreserved, tr.factsTotal, verdict)
			cur := aggPres[tr.tier]
			aggPres[tr.tier] = [2]int{cur[0] + tr.factsPreserved, cur[1] + tr.factsTotal}
		}
	}
	w.Flush()

	var agg strings.Builder
	aw := tabwriter.NewWriter(&agg, 0, 2, 2, ' ', 0)
	fmt.Fprintln(aw, "tier\tfacts_preserved_corpus\tverdict")
	for _, tier := range []Tier{TierFaithful, TierStructured, TierOutline} {
		p := aggPres[tier]
		pct := 0.0
		if p[1] > 0 {
			pct = 100 * float64(p[0]) / float64(p[1])
		}
		verdict := "answer-safe"
		if float64(p[0]) < factPreservationThreshold*float64(p[1]) {
			verdict = "DEGRADING (lossy by design)"
		}
		fmt.Fprintf(aw, "%s\t%d/%d (%.0f%%)\t%s\n", tier, p[0], p[1], pct, verdict)
	}
	aw.Flush()

	t.Logf("\n=== DISTILL conversion proof — savings × answer-relevant fact preservation ===\n%s\n--- aggregate (corpus) ---\n%s", b.String(), agg.String())

	if !anyDegrading {
		t.Error("no tier flagged degrading — a proof that cannot come out negative is not a proof")
	}
}
