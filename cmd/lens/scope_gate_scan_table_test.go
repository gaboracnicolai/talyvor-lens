package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scope_gate_scan_table_test.go — the red-first table for scanScopeGates.
//
// `wantSites` is what is TRUE of the sources; `lineSites` is what the rule this replaced
// answered — count the lines of main.go containing `auth.RequireScope(`, skipping lines that
// START with `//`.

func legacyScopeCount(mainSrc string) int {
	n := 0
	for _, line := range strings.Split(mainSrc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "auth.RequireScope(") {
			n++
		}
	}
	return n
}

type scopeRow struct {
	name      string
	mainSrc   string
	otherName string // an additional file in the package, "" for none
	otherSrc  string
	wantSites int
	lineSites int
}

const oneGate = "package main\n\nfunc run() {\n\tproxyScope := auth.RequireScope(auth.ScopeProxy)\n\t_ = proxyScope\n}\n"

var scopeRows = []scopeRow{
	{
		name: "the one shipped gate", mainSrc: oneGate,
		wantSites: 1, lineSites: 1,
	},
	{
		name:      "a second gate written the same way",
		mainSrc:   strings.Replace(oneGate, "\t_ = proxyScope\n", "\tadminScope := auth.RequireScope(auth.ScopeAdmin)\n\t_ = adminScope\n", 1),
		wantSites: 2, lineSites: 2,
	},
	{
		name:      "a second gate bound through an ALIAS of RequireScope",
		mainSrc:   strings.Replace(oneGate, "\t_ = proxyScope\n", "\trs := auth.RequireScope\n\tadminScope := rs(auth.ScopeAdmin)\n\t_ = adminScope\n", 1),
		wantSites: 2, lineSites: 1,
	},
	{
		name:    "a second gate in ANOTHER FILE of the package",
		mainSrc: oneGate, otherName: "extra.go",
		otherSrc:  "package main\n\nvar extraGate = auth.RequireScope(auth.ScopeAdmin)\n",
		wantSites: 2, lineSites: 1,
	},
	{
		name:    "a gate in a _test.go file — not part of the shipped binary",
		mainSrc: oneGate, otherName: "extra_test.go",
		otherSrc:  "package main\n\nvar testOnlyGate = auth.RequireScope(auth.ScopeAdmin)\n",
		wantSites: 1, lineSites: 1,
	},
	{
		name:      "a TRAILING comment naming the call",
		mainSrc:   strings.Replace(oneGate, "\t_ = proxyScope\n", "\t_ = proxyScope // was auth.RequireScope(auth.ScopeAdmin)\n", 1),
		wantSites: 1, lineSites: 2,
	},
	{
		name:      "a BLOCK comment naming the call",
		mainSrc:   strings.Replace(oneGate, "\t_ = proxyScope\n", "\t/* auth.RequireScope(auth.ScopeAdmin) */\n\t_ = proxyScope\n", 1),
		wantSites: 1, lineSites: 2,
	},
	{
		name:      "the call named inside a STRING literal",
		mainSrc:   strings.Replace(oneGate, "\t_ = proxyScope\n", "\tlogger.Debug(\"auth.RequireScope(auth.ScopeAdmin) is not used\")\n\t_ = proxyScope\n", 1),
		wantSites: 1, lineSites: 2,
	},
	{
		name:    "no gate anywhere",
		mainSrc: "package main\n\nfunc run() {\n}\n",
	},
}

func writeScopeDir(t *testing.T, r scopeRow) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(r.mainSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	if r.otherName != "" {
		if err := os.WriteFile(filepath.Join(dir, r.otherName), []byte(r.otherSrc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestScanScopeGates_ReadsCallsAcrossThePackage(t *testing.T) {
	for _, r := range scopeRows {
		sites, files, err := scanScopeGates(writeScopeDir(t, r))
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		if len(sites) != r.wantSites {
			t.Errorf("%s: sites=%d want %d (%v)", r.name, len(sites), r.wantSites, sites)
		}
		// Order-independent: sites come back in file order, so "the proxy gate is among them"
		// is the property, not "it is first".
		if r.wantSites > 0 {
			seen := false
			for _, x := range sites {
				if x.scope == "auth.ScopeProxy" {
					seen = true
				}
			}
			if !seen {
				t.Errorf("%s: the shipped auth.ScopeProxy gate is not among %v", r.name, sites)
			}
		}
		wantFiles := 1
		if r.otherName != "" && !strings.HasSuffix(r.otherName, "_test.go") {
			wantFiles = 2
		}
		if len(files) != wantFiles {
			t.Errorf("%s: files read=%v want %d — _test.go files are not the shipped binary", r.name, files, wantFiles)
		}
		if ls := legacyScopeCount(r.mainSrc); ls != r.lineSites {
			t.Fatalf("%s: the recorded line-rule answer is stale (got %d want %d)", r.name, ls, r.lineSites)
		}
	}
}

// TestScanScopeGates_TheLineRuleWasWrongOnTheseRows pins how the replaced rule erred in BOTH
// directions: it missed real gates bound through an alias or living in another file, and it
// counted comments and string literals as gates.
func TestScanScopeGates_TheLineRuleWasWrongOnTheseRows(t *testing.T) {
	var missed, falseCount, agree int
	for _, r := range scopeRows {
		ls := legacyScopeCount(r.mainSrc)
		switch {
		case ls < r.wantSites:
			missed++
		case ls > r.wantSites:
			falseCount++
		default:
			agree++
		}
	}
	if missed != 2 {
		t.Errorf("real gates the line rule MISSED = %d, want 2", missed)
	}
	if falseCount != 3 {
		t.Errorf("comments/strings the line rule counted as gates = %d, want 3", falseCount)
	}
	if agree < 3 {
		t.Errorf("only %d rows describe behaviour the line rule got right — a table that is red "+
			"everywhere is not measuring the scanner", agree)
	}
}

// TestScanScopeGates_RecordsWhereAndThroughWhat — the fields the census's failure message is
// built from. Without this the alias and location are carried by nothing and could be dropped
// with everything green, leaving "there are two gates" without "and here is the second".
func TestScanScopeGates_RecordsWhereAndThroughWhat(t *testing.T) {
	dir := writeScopeDir(t, scopeRow{
		mainSrc:   strings.Replace(oneGate, "\t_ = proxyScope\n", "\trs := auth.RequireScope\n\tadminScope := rs(auth.ScopeAdmin)\n\t_ = adminScope\n", 1),
		otherName: "extra.go",
		otherSrc:  "package main\n\nvar extraGate = auth.RequireScope(auth.ScopeKeys)\n",
	})
	sites, _, err := scanScopeGates(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 3 {
		t.Fatalf("sites=%d want 3 (%v)", len(sites), sites)
	}
	byScope := map[string]scopeGateSite{}
	for _, s := range sites {
		byScope[s.scope] = s
	}
	if s := byScope["auth.ScopeAdmin"]; s.alias != "rs" || s.file != "main.go" {
		t.Errorf("the aliased gate = %+v, want alias rs in main.go", s)
	}
	if s := byScope["auth.ScopeKeys"]; s.file != "extra.go" || s.alias != "" {
		t.Errorf("the other-file gate = %+v, want extra.go called directly", s)
	}
	if s := byScope["auth.ScopeProxy"]; s.line == 0 {
		t.Errorf("the shipped gate carries no line number: %+v", s)
	}
}

// TestScanScopeGates_ParseErrorsAreReturned — a file this scan cannot read is a file whose scope
// gates are uncounted, and the census would report a SMALLER number rather than an error.
func TestScanScopeGates_ParseErrorsAreReturned(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc run() { this is not go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sites, files, err := scanScopeGates(dir)
	if err == nil {
		t.Fatalf("unparseable source returned no error (sites=%v files=%v)", sites, files)
	}
	if sites != nil || files != nil {
		t.Errorf("results must be nil on a parse error, got %v %v", sites, files)
	}
}
