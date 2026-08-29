package main

import (
	"regexp"
	"strings"
	"testing"
)

// batch_wiring_scan_table_test.go — the red-first table for scanBatchWiring.
//
// `want*` is what is TRUE of the source; `line*` is what the rules this replaced answered.

// The three replaced rules, verbatim, kept as the things under comparison.
var legacyBatchRouteRe = regexp.MustCompile(`\b(?:authed|pub|r)\.(?:Get|Post|Delete)\("(/v1/batch/[^"]*)"`)

func legacyBareBatch(src string) bool { return len(legacyBatchRouteRe.FindAllString(src, -1)) > 0 }
func legacyGatedSubmit(src string) bool {
	return strings.Contains(src, `batchGate.post(authed, "/v1/batch/submit"`)
}
func legacyLaneClosed(src string) bool {
	return strings.Contains(src, `batchGate := newBatchReg(cfg.BatchEnabled, false)`)
}

type batchRow struct {
	name       string
	body       string
	wantBare   int
	wantGated  int
	wantSettle string
	lineBare   bool
	lineGated  bool
	lineClosed bool
}

const gatedSubmit = "\tbatchGate.post(authed, \"/v1/batch/submit\", newBatchSubmitHandler(batchRouter))\n"
const laneClosed = "\tbatchGate := newBatchReg(cfg.BatchEnabled, false)\n"

var batchRows = []batchRow{
	{
		name: "the shipped shape: closed lane, gated submit", body: laneClosed + gatedSubmit,
		wantGated: 1, wantSettle: "false", lineGated: true, lineClosed: true,
	},
	{
		name: "a BARE batch route, authed.Post, one line", body: "\tauthed.Post(\"/v1/batch/probe\", h)\n",
		wantBare: 1, lineBare: true,
	},
	{
		name: "the same bare route with authed.Put", body: "\tauthed.Put(\"/v1/batch/probe\", h)\n",
		wantBare: 1,
	},
	{
		name: "the same bare route with r.Handle — the unauthenticated root router",
		body: "\tr.Handle(\"/v1/batch/probe\", h)\n", wantBare: 1,
	},
	{
		name: "the same bare route split across lines",
		body: "\tauthed.Post(\n\t\t\"/v1/batch/probe\",\n\t\th,\n\t)\n", wantBare: 1,
	},
	{
		name: "a bare route that is COMMENTED OUT — it binds nothing",
		body: "\t// authed.Post(\"/v1/batch/probe\", h)\n\t_ = h\n", lineBare: true,
	},
	{
		name:      "the gated registration exists only as a COMMENT",
		body:      "\t// batchGate.post(authed, \"/v1/batch/submit\", newBatchSubmitHandler(batchRouter))\n\t_ = h\n",
		lineGated: true,
	},
	{
		name: "the lane OPENED", body: "\tbatchGate := newBatchReg(cfg.BatchEnabled, true)\n",
		wantSettle: "true",
	},
	{
		name:       "the lane OPENED with the old closed text left in a COMMENT",
		body:       "\t// batchGate := newBatchReg(cfg.BatchEnabled, false)\n\tbatchGate := newBatchReg(cfg.BatchEnabled, true)\n",
		wantSettle: "true", lineClosed: true,
	},
	{
		name:       "the lane still CLOSED, written across lines",
		body:       "\tbatchGate := newBatchReg(\n\t\tcfg.BatchEnabled,\n\t\tfalse,\n\t)\n",
		wantSettle: "false",
	},
}

func TestScanBatchWiring_ReadsCallsNotText(t *testing.T) {
	for _, r := range batchRows {
		src := wrapMain(r.body)
		w, err := scanBatchWiring("synthetic.go", []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		if len(w.bare) != r.wantBare {
			t.Errorf("%s: bare=%d want %d (%v)", r.name, len(w.bare), r.wantBare, w.bare)
		}
		if len(w.gated) != r.wantGated {
			t.Errorf("%s: gated=%d want %d (%v)", r.name, len(w.gated), r.wantGated, w.gated)
		}
		if w.settleWired != r.wantSettle {
			t.Errorf("%s: settleWired=%q want %q", r.name, w.settleWired, r.wantSettle)
		}
		lb, lg, lc := legacyBareBatch(src), legacyGatedSubmit(src), legacyLaneClosed(src)
		if lb != r.lineBare || lg != r.lineGated || lc != r.lineClosed {
			t.Fatalf("%s: a recorded legacy answer is stale (bare %v/%v gated %v/%v closed %v/%v)",
				r.name, lb, r.lineBare, lg, r.lineGated, lc, r.lineClosed)
		}
	}
}

// TestScanBatchWiring_TheTextRulesWereWrongOnTheseRows pins the errors in BOTH directions: real
// bare routes and a real lane-opening missed, and comments counted as code.
func TestScanBatchWiring_TheTextRulesWereWrongOnTheseRows(t *testing.T) {
	var missedBare, missedOpen, falseFromComment, agree int
	for _, r := range batchRows {
		src := wrapMain(r.body)
		lb, lg, lc := legacyBareBatch(src), legacyGatedSubmit(src), legacyLaneClosed(src)
		switch {
		case r.wantBare > 0 && !lb:
			missedBare++
		case r.wantSettle == "true" && lc: // the guard returns early and enforces nothing
			missedOpen++
		case (r.wantBare == 0 && lb) || (r.wantGated == 0 && lg) || (r.wantSettle == "false" && !lc):
			falseFromComment++
		default:
			agree++
		}
	}
	if missedBare != 3 {
		t.Errorf("bare batch routes the regex MISSED = %d, want 3", missedBare)
	}
	if missedOpen != 1 {
		t.Errorf("lane-openings the text rule MISSED = %d, want 1", missedOpen)
	}
	if falseFromComment != 3 {
		t.Errorf("rows the text rules answered from a comment or a reformat = %d, want 3", falseFromComment)
	}
	if agree < 3 {
		t.Errorf("only %d rows describe behaviour the text rules got right — a table that is red "+
			"everywhere is not measuring the scanner", agree)
	}
}

// TestScanBatchWiring_RecordsTheHandlerAndTheVerb — the fields the gated-route assertions are
// built from; without these the scan could report "a gate registration exists" while saying
// nothing about which route or which handler, which is the whole point of that assertion.
func TestScanBatchWiring_RecordsTheHandlerAndTheVerb(t *testing.T) {
	w, err := scanBatchWiring("synthetic.go", []byte(wrapMain(gatedSubmit+
		"\tbatchGate.get(authed, \"/v1/batch/jobs\", listHandler)\n")))
	if err != nil {
		t.Fatal(err)
	}
	g, ok := w.gatedRoute("post", "/v1/batch/submit")
	if !ok || g.handler != "newBatchSubmitHandler(batchRouter)" || g.line == 0 {
		t.Errorf("submit gate registration = %+v (found=%v), want the newBatchSubmitHandler call with a line", g, ok)
	}
	if _, ok := w.gatedRoute("get", "/v1/batch/submit"); ok {
		t.Error("the VERB is part of the question — a get must not satisfy a post")
	}
	// ⚠ AND SO IS THE GATE. Without this the receiver check is asserted by nothing: a control
	// that accepted ANY receiver changed no verdict, so "through batchGate" could have been
	// relaxed to "through anything with a three-argument method" with everything green — and
	// the whole point of these guards is that the lane is reachable only through the gate.
	other, err := scanBatchWiring("synthetic.go", []byte(wrapMain(
		"\totherReg.post(authed, \"/v1/batch/submit\", newBatchSubmitHandler(batchRouter))\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(other.gated) != 0 {
		t.Errorf("a registration through otherReg was counted as gated: %v", other.gated)
	}
	if g, ok := w.gatedRoute("get", "/v1/batch/jobs"); !ok || g.handler != "listHandler" {
		t.Errorf("jobs gate registration = %+v (found=%v)", g, ok)
	}
}

// TestScanBatchWiring_ParseErrorsAreReturned — a scan that finds no bare routes reports the lane
// safe, which is the wrong direction to be wrong in.
func TestScanBatchWiring_ParseErrorsAreReturned(t *testing.T) {
	w, err := scanBatchWiring("broken.go", []byte("package main\n\nfunc run() { this is not go\n"))
	if err == nil {
		t.Fatalf("unparseable source returned no error (w=%v)", w)
	}
	if w != nil {
		t.Errorf("result must be nil on a parse error, got %v", w)
	}
}
