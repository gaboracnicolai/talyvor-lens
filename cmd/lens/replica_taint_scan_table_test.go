package main

import (
	"strings"
	"testing"
)

// replica_taint_scan_table_test.go — the red-first table for scanReplicaWiring.
//
// `wantViolation` is what is TRUE of the source; `lineViolation` is what the LINE rule
// this replaced answered. Rows where they disagree are the defect; rows where they agree
// must keep passing on BOTH sides, or the table is measuring nothing.

// legacyLineViolation is TestReadReplicaWiring_MoneyAuthzNeverReceiveReplica's rule
// verbatim, kept as the thing under comparison.
func legacyLineViolation(src, ctor string) bool {
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "replicaPool") && !strings.Contains(line, "dbrouting.ReadPool") {
			continue
		}
		if strings.Contains(line, ctor+"(") {
			return true
		}
	}
	return false
}

type replicaRow struct {
	name          string
	body          string
	wantViolation bool
	lineViolation bool
}

var replicaRows = []replicaRow{
	{
		name: "authz store on the primary — correct wiring",
		body: "\tkeyStore := auth.New(pool)\n",
	},
	{
		name:          "authz store takes the replica, one line",
		body:          "\tkeyStore := auth.New(replicaPool)\n",
		wantViolation: true,
		lineViolation: true,
	},
	{
		name:          "authz store takes the replica, split across lines",
		body:          "\tkeyStore := auth.New(\n\t\treplicaPool,\n\t)\n",
		wantViolation: true,
	},
	{
		name:          "authz store takes the replica through a one-step alias",
		body:          "\tauthPool := replicaPool\n\tkeyStore := auth.New(authPool)\n",
		wantViolation: true,
	},
	{
		name:          "…and through a two-step alias chain",
		body:          "\tp1 := replicaPool\n\tp2 := p1\n\tkeyStore := auth.New(p2)\n",
		wantViolation: true,
	},
	{
		name:          "authz store takes ReadPool(), one line",
		body:          "\tkeyStore := auth.New(dbrouting.ReadPool(pool, replicaPool))\n",
		wantViolation: true,
		lineViolation: true,
	},
	{
		name:          "authz store takes ReadPool(), split across lines",
		body:          "\tkeyStore := auth.New(\n\t\tdbrouting.ReadPool(pool, replicaPool),\n\t)\n",
		wantViolation: true,
	},
	{
		name:          "authz store takes ReadPool() through an alias",
		body:          "\treadOnly := dbrouting.ReadPool(pool, replicaPool)\n\tkeyStore := auth.New(readOnly)\n",
		wantViolation: true,
	},
	{
		name: "an ANALYTICS reader on the replica — allowed, not a money constructor",
		body: "\tcostAnomalyStore := costanomaly.NewStore(dbrouting.ReadPool(pool, replicaPool))\n",
	},
	{
		name:          "a COMMENT describing the forbidden wiring",
		body:          "\t// never do this: auth.New(replicaPool)\n\tkeyStore := auth.New(pool)\n",
		lineViolation: true,
	},
	{
		name:          "the forbidden wiring quoted inside a STRING",
		body:          "\tlogger.Debug(\"refused: auth.New(replicaPool)\")\n\tkeyStore := auth.New(pool)\n",
		lineViolation: true,
	},
	{
		name: "the replica is mentioned on the line but goes nowhere near the constructor",
		body: "\t_ = replicaPool\n\tkeyStore := auth.New(pool)\n",
	},
}

func TestScanReplicaWiring_FollowsArgumentsNotLines(t *testing.T) {
	for _, r := range replicaRows {
		src := wrapMain(r.body)
		scan, err := scanReplicaWiring("synthetic.go", []byte(src))
		if err != nil {
			t.Fatalf("%s: scanReplicaWiring: %v", r.name, err)
		}
		got := false
		for _, c := range scan.calls {
			if c.name == "auth.New" && c.replicaArg != "" {
				got = true
			}
		}
		if got != r.wantViolation {
			t.Errorf("%s: violation=%v want %v", r.name, got, r.wantViolation)
		}
		if lv := legacyLineViolation(src, "auth.New"); lv != r.lineViolation {
			t.Fatalf("%s: the recorded line-rule answer is stale (got %v want %v)", r.name, lv, r.lineViolation)
		}
	}
}

// TestScanReplicaWiring_TheLineRuleWasWrongOnTheseRows pins how the replaced rule erred:
// it MISSED real violations written across lines or through an alias, and it FALSELY
// accused comments and strings. Both counts are recorded so neither can drift silently,
// and the agreeing rows prove the table is not uniformly red.
func TestScanReplicaWiring_TheLineRuleWasWrongOnTheseRows(t *testing.T) {
	var missed, falseAccusation, agree int
	for _, r := range replicaRows {
		lv := legacyLineViolation(wrapMain(r.body), "auth.New")
		switch {
		case r.wantViolation && !lv:
			missed++
		case !r.wantViolation && lv:
			falseAccusation++
		default:
			agree++
		}
	}
	if missed != 5 {
		t.Errorf("real violations the line rule MISSED = %d, want 5", missed)
	}
	if falseAccusation != 2 {
		t.Errorf("comments/strings the line rule falsely accused = %d, want 2", falseAccusation)
	}
	if agree < 4 {
		t.Errorf("only %d rows describe behaviour the line rule got right — a table that is "+
			"red everywhere is not measuring the scanner", agree)
	}
}

// TestScanReplicaWiring_CountsCallsNotText — the sibling count guard's question. A
// commented-out wiring line must not restore a reader that was removed from the replica.
func TestScanReplicaWiring_CountsCallsNotText(t *testing.T) {
	real := wrapMain("\ta := costanomaly.NewStore(dbrouting.ReadPool(pool, replicaPool))\n")
	commented := wrapMain("\t// a := costanomaly.NewStore(dbrouting.ReadPool(pool, replicaPool))\n\ta := costanomaly.NewStore(pool)\n")
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{{"a real call", real, 1}, {"only a commented-out call", commented, 0}} {
		scan, err := scanReplicaWiring("synthetic.go", []byte(tc.src))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if scan.readPoolCalls != tc.want {
			t.Errorf("%s: readPoolCalls=%d want %d", tc.name, scan.readPoolCalls, tc.want)
		}
		ok, err := scan.wraps("costanomaly.NewStore", readPoolFunc, []byte(tc.src), "synthetic.go")
		if err != nil {
			t.Fatalf("%s: wraps: %v", tc.name, err)
		}
		if ok != (tc.want == 1) {
			t.Errorf("%s: wraps=%v want %v", tc.name, ok, tc.want == 1)
		}
	}
}

// TestScanReplicaWiring_ParseErrorsAreReturned — a scan that answers "no violations"
// because the file did not parse is a guard that cannot fail.
func TestScanReplicaWiring_ParseErrorsAreReturned(t *testing.T) {
	scan, err := scanReplicaWiring("broken.go", []byte("package main\n\nfunc run() { this is not go\n"))
	if err == nil {
		t.Fatalf("unparseable source returned no error (scan=%v)", scan)
	}
	if scan != nil {
		t.Errorf("scan must be nil on a parse error, got %v", scan)
	}
}

// TestScanReplicaWiring_TaintIsSeededAndClosed — the two halves that make the alias rows
// work, asserted directly so neither can be removed without a red.
func TestScanReplicaWiring_TaintIsSeededAndClosed(t *testing.T) {
	scan, err := scanReplicaWiring("synthetic.go", []byte(wrapMain("\tp1 := replicaPool\n\tp2 := p1\n\t_ = p2\n")))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{replicaPoolIdent, "p1", "p2"} {
		if !scan.tainted[id] {
			t.Errorf("%q is not tainted — the replica would not be followed through it", id)
		}
	}
	if scan.tainted["pool"] {
		t.Error("the PRIMARY pool must not be tainted, or every constructor is a violation")
	}
}

// ⚠ A STATED LIMIT, NOT A DISCOVERED ONE. The taint set follows IDENTIFIERS. A replica
// pool stashed in a struct field or a map and read back out is not followed, and such a
// call would be reported CLEAN. No wiring in main.go does that today; if one appears, this
// scanner must grow before it is trusted over it. That is the under-reporting direction,
// which is the unsafe one — said here rather than left to be found.
