package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/doc2query"
	"github.com/talyvor/lens/internal/embedder"
	"github.com/talyvor/lens/internal/poolsafety"
)

// ⚠ THE COST W2.7 ASKS FOR IS NOT THE COST THIS HARNESS SPENDS, AND CONFLATING THEM OVERSTATES
// doc2query BY ROUGHLY AN ORDER OF MAGNITUDE.
//
// The harness generates an answer for every corpus question because doc2query derives from answers
// and measuring it without real ones would measure a different system. In production that answer
// already exists — it is the reply the user paid for. So the marginal write-time cost of turning
// doc2query ON is the DERIVE call plus the VARIANT embeddings, and nothing else. The break-even in
// the package doc ("one extra hit for every seven answers stored") is computed against that
// marginal figure; charging the generation to it would move break-even by ~10x and quietly change
// the verdict.
//
// This drives the whole accounting path over real HTTP round trips against fakes, and asserts the
// four buckets separately. Numbers are distinct and non-round so a meter that added the wrong pair
// of them cannot land on the right answer.
func TestCostMeter_SeparatesTheHarnessFixtureFromDoc2querysMarginalCost(t *testing.T) {
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"text":"how do I center a div?\nwhat causes a segfault?"}],` +
			`"usage":{"input_tokens":817,"output_tokens":63}}`))
	}))
	defer anth.Close()
	oai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.6,0.8]}],"usage":{"prompt_tokens":13,"total_tokens":13}}`))
	}))
	defer oai.Close()

	m := &costMeter{}
	emb := &meteredEmbedder{e: embedder.NewOpenAIEmbedder("k", "text-embedding-3-small", oai.URL), m: m}
	der := doc2query.NewAnthropicDeriver("k", "claude-haiku-4-5")
	der.BaseURL = anth.URL

	pairs := []poolsafety.RephrasePair{{Name: "one", A: "a?", B: "b?"}}
	res, err := measureCorpus(t.Context(), emb, der, der, pairs, 8, m)
	if err != nil {
		t.Fatalf("measureCorpus: %v", err)
	}
	if res[0].NumVars != 2 {
		t.Fatalf("NumVars = %d, want 2 — the variant embeddings below are counted per variant, so "+
			"a different parse makes every count here wrong for a reason unrelated to the meter",
			res[0].NumVars)
	}

	// 1 answer, 1 derive, and 4 embeddings: B, A, and one per derived variant.
	if m.answerCalls != 1 || m.answerUsage != (doc2query.Usage{InTokens: 817, OutTokens: 63}) {
		t.Errorf("answer bucket = %d calls %+v, want 1 call {817 63}", m.answerCalls, m.answerUsage)
	}
	if m.deriveCalls != 1 || m.deriveUsage != (doc2query.Usage{InTokens: 817, OutTokens: 63}) {
		t.Errorf("derive bucket = %d calls %+v, want 1 call {817 63}", m.deriveCalls, m.deriveUsage)
	}
	if m.embedCalls != 4 || m.embedTokens != 52 {
		t.Errorf("embed bucket = %d calls / %d tokens, want 4 / 52", m.embedCalls, m.embedTokens)
	}

	// ⚠ AND THE REPORT MUST SAY WHICH IS WHICH. A total that is right and unlabelled is read as
	// doc2query's cost by whoever quotes it next.
	var buf bytes.Buffer
	m.report(&buf, 2, 1)
	out := buf.String()
	for _, want := range []string{
		"HARNESS FIXTURE, not doc2query's cost",
		"doc2query's marginal write-time cost",
		"per stored answer: 817.0 in · 63.0 out",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cost report does not contain %q\n---\n%s", want, out)
		}
	}
}
