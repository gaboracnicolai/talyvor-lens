package tare_test

// corpus_test.go — W6.1.1's ACTUAL DELIVERABLE: what the JSON compressor measures on real content.
//
// ⚠ THE ITEM'S OWN WARNING IS THE REASON THIS FILE EXISTS: "the previous compressor measured 0.000%
// over 308 prompts and shipped anyway." A number is only worth reporting alongside the POPULATION
// it was measured over, so this file reports both, per corpus, and asserts a floor on each
// population so an empty corpus can never pass silently.
//
// ⚠ AND IT MEASURES THE COMPOSITION BEFORE THE SAVING. W6.1.1 says to use "the committed corpora in
// internal/poolsafety plus real JSON tool outputs". The poolsafety corpora are PROMPT PAIRS — short
// natural-language questions built to test similarity, not tool output — so a JSON compressor
// scores exactly zero on them BY CONSTRUCTION, not by being bad. Reporting "0.00% over 308 prompts"
// without that sentence is how the previous compressor's number came to mean nothing. So the first
// assertion here is how many of those entries are JSON at all.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
	"github.com/talyvor/lens/internal/tare"
)

type corpusResult struct {
	name       string
	entries    int
	parsedJSON int
	reduced    int
	bytesIn    int
	bytesOut   int
}

func (c corpusResult) pct() float64 {
	if c.bytesIn == 0 {
		return 0
	}
	return 100 * float64(c.bytesIn-c.bytesOut) / float64(c.bytesIn)
}

// runCorpus reduces every entry, ROUND-TRIPS every result, and tallies.
//
// ⚠ THE ROUND-TRIP IS NOT OPTIONAL AND IT IS THE POINT OF USING REAL DATA. Synthetic tests prove
// the transform is invertible on shapes I thought of. Real payloads are how you find the shape you
// did not.
func runCorpus(t *testing.T, name string, entries [][]byte) corpusResult {
	t.Helper()
	r := tare.NewJSONReducer()
	res := corpusResult{name: name, entries: len(entries)}
	for i, in := range entries {
		var probe any
		if json.Unmarshal(in, &probe) == nil {
			res.parsedJSON++
		}
		out, _, _, err := r.Reduce(context.Background(), in, tare.KindJSON)
		if err != nil {
			t.Fatalf("%s[%d]: Reduce returned an error, which a refusal must never be: %v", name, i, err)
		}
		res.bytesIn += len(in)
		res.bytesOut += len(out)
		if len(out) != len(in) {
			res.reduced++
			mustRoundTrip(t, in, out)
		}
	}
	return res
}

// ⚠ THE POOLSAFETY CORPORA ARE PROSE. Measured, so nobody has to take my word for it.
func TestCorpus_PoolsafetyPromptsAreNotJSONAndScoreStructurallyZero(t *testing.T) {
	var entries [][]byte
	for _, set := range [][]poolsafety.RephrasePair{
		poolsafety.EngineeringRephrasePairs(),
		poolsafety.EngineeringDangerPairs(),
		poolsafety.HeldOutDangerPairs(),
		poolsafety.ConsumerDangerPairs(),
		poolsafety.ConsumerRephrasePairs(),
		poolsafety.ConsumerUnrelatedPairs(),
	} {
		for _, p := range set {
			entries = append(entries, []byte(p.A), []byte(p.B))
		}
	}

	// A floor on the population: a corpus that reads as empty would make every number below
	// vacuously true, which is precisely how a meaningless 0.000% gets published.
	if len(entries) < 100 {
		t.Fatalf("the poolsafety corpora yielded %d prompts — that is not the committed corpus, and "+
			"a reduction figure over it would be a statement about the loader", len(entries))
	}

	res := runCorpus(t, "poolsafety prompts", entries)
	t.Logf("MEASURED — %s: %d entries, %d parsed as JSON, %d reduced, %d -> %d bytes (%.3f%%)",
		res.name, res.entries, res.parsedJSON, res.reduced, res.bytesIn, res.bytesOut, res.pct())

	if res.reduced != 0 {
		t.Fatalf("%d prose prompts were 'reduced' by a JSON compressor — that would mean it is "+
			"transforming something it does not understand", res.reduced)
	}
	// ⚠ THE HONEST STATEMENT OF THE RESULT. Not "the compressor achieved 0%" — "the corpus contains
	// nothing this compressor is for". The distinction is the whole finding.
	if res.parsedJSON > len(entries)/10 {
		t.Fatalf("%d of %d poolsafety prompts parse as JSON. If that is now true, this corpus IS "+
			"partly JSON and the 0%% result above needs re-reading", res.parsedJSON, len(entries))
	}
}

// ⚠ REAL JSON FROM THE REPO — and labelled for what it is.
func TestCorpus_RealJSONInThisRepo(t *testing.T) {
	root := "../.."
	var files []string
	for _, pat := range []string{
		"deploy/observability/grafana/*.json",
		"deploy/helm/lens/*.json",
		"internal/distill/testdata/*.json",
		"internal/distill/testdata/*/*.json",
		"sdk/typescript/package-lock.json",
	} {
		m, _ := filepath.Glob(filepath.Join(root, pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	if len(files) < 4 {
		t.Fatalf("found %d real JSON files — the glob has gone blind and a figure over it would be "+
			"a statement about the search", len(files))
	}

	var total corpusResult
	total.name = "all real JSON"
	var perFile []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		res := runCorpus(t, filepath.Base(f), [][]byte{b})
		total.entries += res.entries
		total.parsedJSON += res.parsedJSON
		total.reduced += res.reduced
		total.bytesIn += res.bytesIn
		total.bytesOut += res.bytesOut
		perFile = append(perFile, fmt.Sprintf("  %-34s %8d -> %8d  %6.2f%%  %s",
			filepath.Base(f), res.bytesIn, res.bytesOut, res.pct(),
			map[bool]string{true: "reduced", false: "REFUSED"}[res.reduced > 0]))
	}
	t.Logf("MEASURED — real JSON in this repo, per file:\n%s\n  %-34s %8d -> %8d  %6.2f%%",
		strings.Join(perFile, "\n"), "TOTAL", total.bytesIn, total.bytesOut, total.pct())

	if total.bytesIn < 100_000 {
		t.Fatalf("the real-JSON corpus is only %d bytes — too small for the percentage to mean "+
			"anything", total.bytesIn)
	}

	// ⚠ NO THRESHOLD IS ASSERTED HERE ON PURPOSE. W6.1.1 says to REPORT the measured reduction and
	// to say so if it is near zero — not to pass a bar. A test that failed below some percentage
	// would be a test of the corpus, and the next person would tune the corpus.
	t.Logf("⚠ POPULATION BOUNDARY, STATED: these are Grafana dashboards, a Helm values schema, " +
		"distill fixtures and an npm lockfile. They are REAL JSON but they are NOT agent tool " +
		"output, which is what Tare would actually see. This repo captures no gateway traffic, so " +
		"the representative corpus does not exist yet — and the figure above is therefore an " +
		"indication of the transform's behaviour on dict-array-heavy JSON, NOT a forecast of " +
		"production saving. Building that corpus is the first thing Phase 1d needs.")
}

// ⚠ A SHAPE-BY-SHAPE PROBE, so the headline number can be read rather than just quoted. Which
// shapes does this transform actually pay on?
func TestCorpus_ReductionByShape(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"tool output: 20 same-shaped rows", buildRows(20, `{"path":"pkg/mod/file_%d.go","line":%d,"col":4,"rule":"unused","severity":"warning"}`)},
		{"tool output: 200 same-shaped rows", buildRows(200, `{"path":"pkg/mod/file_%d.go","line":%d,"col":4,"rule":"unused","severity":"warning"}`)},
		{"3 rows, short keys", `[{"a":1,"b":2},{"a":3,"b":4},{"a":5,"b":6}]`},
		{"3 rows, long keys", `[{"aVeryDescriptiveFieldName":1,"anotherDescriptiveName":2},{"aVeryDescriptiveFieldName":3,"anotherDescriptiveName":4},{"aVeryDescriptiveFieldName":5,"anotherDescriptiveName":6}]`},
		{"mixed shapes (refused)", `[{"a":1},{"a":1,"b":2},{"a":3}]`},
		{"scalar array (refused)", `[1,2,3,4,5,6,7,8,9,10]`},
		{"deeply nested object, no arrays (refused)", `{"a":{"b":{"c":{"d":{"e":1}}}}}`},
	}
	r := tare.NewJSONReducer()
	var lines []string
	for _, c := range cases {
		out, _, _, err := r.Reduce(context.Background(), []byte(c.in), tare.KindJSON)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(out) != len(c.in) {
			mustRoundTrip(t, []byte(c.in), out)
		}
		pct := 100 * float64(len(c.in)-len(out)) / float64(len(c.in))
		lines = append(lines, fmt.Sprintf("  %-42s %7d -> %7d  %6.2f%%", c.name, len(c.in), len(out), pct))
	}
	t.Logf("MEASURED — reduction by shape:\n%s", strings.Join(lines, "\n"))
}

func buildRows(n int, tmpl string) string {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf(tmpl, i, i*7+1))
	}
	return "[" + strings.Join(parts, ",") + "]"
}
