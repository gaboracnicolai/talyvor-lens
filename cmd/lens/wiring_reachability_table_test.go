package main

import (
	"strings"
	"testing"
)

// wiring_reachability_table_test.go — the red-first table for scanWiring.
//
// `wantUncond` is what is TRUE of the source: the hook runs whenever run() runs. `lineUncond`
// is what the "exactly one leading tab" rule this replaced answered.

const hookRecv, hookMethod = "tokenLedger", "SetMintVerifier"

// legacyTabRule is the replaced rule verbatim: present if the text appears anywhere,
// unconditional if some line starts with one tab followed by the receiver.method call.
func legacyTabRule(src string) (present, unconditional bool) {
	for _, ln := range strings.Split(src, "\n") {
		if strings.Contains(ln, hookMethod+"(") {
			present = true
			if strings.HasPrefix(ln, "\t"+hookRecv+"."+hookMethod+"(") {
				unconditional = true
			}
		}
	}
	return present, unconditional
}

// wrapRun builds a synthetic main.go: run()'s body, plus any extra top-level declarations.
func wrapRun(body, extra string) string {
	return "package main\n\nfunc run() {\n" + body + "}\n" + extra
}

const helperDecl = "\nfunc wireMintSafety(tokenLedger *L) {\n\ttokenLedger.SetMintVerifier(v)\n}\n"

type wiringRow struct {
	name       string
	body       string
	extra      string
	wantPres   bool
	wantUncond bool
	linePres   bool
	lineUncond bool
}

var wiringRows = []wiringRow{
	{
		name: "called directly on run()'s straight line", body: "\ttokenLedger.SetMintVerifier(v)\n",
		wantPres: true, wantUncond: true, linePres: true, lineUncond: true,
	},
	{
		name:     "moved inside `if cfg.EconomyEnabled` — the kill lifts it",
		body:     "\tif cfg.EconomyEnabled {\n\t\ttokenLedger.SetMintVerifier(v)\n\t}\n",
		wantPres: true, linePres: true,
	},
	{
		name: "in a helper called UNCONDITIONALLY — still unconditional",
		body: "\twireMintSafety(tokenLedger)\n", extra: helperDecl,
		wantPres: true, wantUncond: true, linePres: true, lineUncond: true,
	},
	{
		name: "in a helper called only `if cfg.EconomyEnabled` — the kill lifts it",
		body: "\tif cfg.EconomyEnabled {\n\t\twireMintSafety(tokenLedger)\n\t}\n", extra: helperDecl,
		wantPres: true, linePres: true, lineUncond: true,
	},
	{
		name: "in a helper that is NEVER called — the floor never enforces",
		body: "\t_ = tokenLedger\n", extra: helperDecl,
		wantPres: true, linePres: true, lineUncond: true,
	},
	{
		name:     "no wiring at all; the call text lives in a RAW STRING starting with one tab",
		body:     "\t_ = tokenLedger\n",
		extra:    "\nconst doc = `boot wiring:\n\ttokenLedger.SetMintVerifier(v)\n`\n",
		linePres: true, lineUncond: true,
	},
	{
		name:     "inside a goroutine literal — deferred, not a boot statement",
		body:     "\tgo func() {\n\t\ttokenLedger.SetMintVerifier(v)\n\t}()\n",
		wantPres: true, linePres: true,
	},
	{
		name:     "inside a for loop in run()",
		body:     "\tfor i := 0; i < 1; i++ {\n\t\ttokenLedger.SetMintVerifier(v)\n\t}\n",
		wantPres: true, linePres: true,
	},
	{
		name:     "reached two hops from run(), both unconditional",
		body:     "\touter(tokenLedger)\n",
		extra:    "\nfunc outer(tokenLedger *L) {\n\twireMintSafety(tokenLedger)\n}\n" + helperDecl,
		wantPres: true, wantUncond: true, linePres: true, lineUncond: true,
	},
	{
		name:     "reached two hops, the SECOND hop conditional",
		body:     "\touter(tokenLedger)\n",
		extra:    "\nfunc outer(tokenLedger *L) {\n\tif cfg.EconomyEnabled {\n\t\twireMintSafety(tokenLedger)\n\t}\n}\n" + helperDecl,
		wantPres: true, linePres: true, lineUncond: true,
	},
	{
		name: "not wired anywhere",
		body: "\t_ = tokenLedger\n",
	},
}

func TestScanWiring_ResolvesReachabilityNotIndentation(t *testing.T) {
	for _, r := range wiringRows {
		src := wrapRun(r.body, r.extra)
		w, err := scanWiring("synthetic.go", []byte(src), map[string]bool{hookMethod: true})
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		if p := w.present(hookRecv, hookMethod); p != r.wantPres {
			t.Errorf("%s: present=%v want %v", r.name, p, r.wantPres)
		}
		if u := w.unconditional(hookRecv, hookMethod); u != r.wantUncond {
			t.Errorf("%s: unconditional=%v want %v (sites %v, reached %v)", r.name, u, r.wantUncond, w.sites, w.reached)
		}
		lp, lu := legacyTabRule(src)
		if lp != r.linePres || lu != r.lineUncond {
			t.Fatalf("%s: the recorded tab-rule answer is stale (present %v want %v, uncond %v want %v)",
				r.name, lp, r.linePres, lu, r.lineUncond)
		}
	}
}

// TestScanWiring_TheTabRuleWasWrongOnTheseRows pins how the replaced rule erred. Every error is
// in the SAME direction — it called a liftable or absent safety restriction unconditional —
// which is the direction that hides the defect the guards exist to prevent.
func TestScanWiring_TheTabRuleWasWrongOnTheseRows(t *testing.T) {
	var falselyUnconditional, agree int
	for _, r := range wiringRows {
		_, lu := legacyTabRule(wrapRun(r.body, r.extra))
		if lu != r.wantUncond {
			if !lu {
				t.Fatalf("%s: the tab rule under-reported, which this table does not expect", r.name)
			}
			falselyUnconditional++
			continue
		}
		agree++
	}
	if falselyUnconditional != 4 {
		t.Errorf("rows the tab rule falsely called unconditional = %d, want 4", falselyUnconditional)
	}
	if agree < 5 {
		t.Errorf("only %d rows describe behaviour the tab rule got right — a table that is red "+
			"everywhere is not measuring the scanner", agree)
	}
}

// TestScanWiring_TheReceiverIsPartOfTheQuestion — main.go calls SetOwnerLinkageCheck on BOTH
// royaltyMinter and distillMinter, and the guard that pins it names royaltyMinter. Matching on
// the method alone would let one site satisfy the other's assertion.
func TestScanWiring_TheReceiverIsPartOfTheQuestion(t *testing.T) {
	src := wrapRun("\tdistillMinter.SetOwnerLinkageCheck(true)\n", "")
	w, err := scanWiring("synthetic.go", []byte(src), map[string]bool{"SetOwnerLinkageCheck": true})
	if err != nil {
		t.Fatal(err)
	}
	if !w.unconditional("distillMinter", "SetOwnerLinkageCheck") {
		t.Error("distillMinter's own call must satisfy distillMinter's assertion")
	}
	if w.present("royaltyMinter", "SetOwnerLinkageCheck") {
		t.Error("royaltyMinter is not wired here — another receiver's call must not satisfy it")
	}
	// ⚠ BOTH HALVES, because only `present` observed the receiver at first: a control that
	// dropped the receiver from `unconditional` changed no verdict, so that half of the rule was
	// asserted by nothing and could have been relaxed away with everything green.
	if w.unconditional("royaltyMinter", "SetOwnerLinkageCheck") {
		t.Error("royaltyMinter has no call site here — distillMinter's must not report it as " +
			"unconditionally wired")
	}
}

// TestScanWiring_ParseErrorsAreReturned — a scan that finds no call sites reports every hook as
// missing, and one that resolves no reachability reports every hook as conditional.
func TestScanWiring_ParseErrorsAreReturned(t *testing.T) {
	w, err := scanWiring("broken.go", []byte("package main\n\nfunc run() { this is not go\n"), map[string]bool{hookMethod: true})
	if err == nil {
		t.Fatalf("unparseable source returned no error (scan=%v)", w)
	}
	if w != nil {
		t.Errorf("scan must be nil on a parse error, got %v", w)
	}
}
