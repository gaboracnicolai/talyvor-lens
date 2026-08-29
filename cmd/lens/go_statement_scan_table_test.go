package main

import (
	"regexp"
	"strings"
	"testing"
)

// go_statement_scan_table_test.go — the red-first table for scanGoStatements (#528).

// legacyAnyGoroutine and legacyLeaderGated are the per-line rules this replaced, kept as the
// things under comparison.
var (
	legacyAnyGoroutine = regexp.MustCompile(`^\s*go `)
	legacyLeaderGated  = regexp.MustCompile(`^\s*go haComps\.leader\.Run\(ctx, "([a-z0-9-]+)"`)
)

// legacyCensus returns (seen, gated) for the goroutine in src, as the line rules answered.
func legacyCensus(src string) (seen, gated bool) {
	for _, line := range strings.Split(src, "\n") {
		if !legacyAnyGoroutine.MatchString(line) {
			continue
		}
		seen = true
		if legacyLeaderGated.MatchString(line) {
			gated = true
		}
	}
	return seen, gated
}

func TestScanGoStatements_SeesEveryGoStatementWhereverItSits(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantSites  int
		wantGated  bool
		legacySeen bool
	}{
		{"at the start of its line", "\tgo logger.Info(\"x\")\n", 1, false, true},
		{"indented inside a block", "\tif c {\n\t\tgo logger.Info(\"x\")\n\t}\n", 1, false, true},
		{
			// ⚠ THE SILENT ONE. The line rule does not see this goroutine at all — not as
			// unclassified, not as anything. It never enters the population.
			name: "NOT at the start of its line — one-line if",
			body: "\tif c { go logger.Info(\"x\") }\n", wantSites: 1, legacySeen: false,
		},
		{"NOT at the start of its line — one-line for", "\tfor i := 0; i < 1; i++ { go logger.Info(\"x\") }\n", 1, false, false},
		{"a leader job on one line", "\tgo haComps.leader.Run(ctx, \"j\", d, func(lctx context.Context) {})\n", 1, true, true},
		{
			// The line rule called this UNCLASSIFIED — a false accusation of correct code.
			name:      "a leader job written ACROSS LINES",
			body:      "\tgo haComps.leader.Run(\n\t\tctx, \"j\", d, func(lctx context.Context) {},\n\t)\n",
			wantSites: 1, wantGated: true, legacySeen: true,
		},
		{"only a COMMENT starts one", "\t// go logger.Info(\"x\")\n\t_ = logger\n", 0, false, false},
		{"no goroutine at all", "\t_ = logger\n", 0, false, false},
	} {
		src := wrapMain(tc.body)
		sites, err := scanGoStatements("synthetic.go", []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(sites) != tc.wantSites {
			t.Errorf("%s: sites=%d want %d (%v)", tc.name, len(sites), tc.wantSites, sites)
			continue
		}
		if tc.wantSites == 1 && (sites[0].jobName != "") != tc.wantGated {
			t.Errorf("%s: gated=%v want %v (jobName %q)", tc.name, sites[0].jobName != "", tc.wantGated, sites[0].jobName)
		}
		if seen, _ := legacyCensus(src); seen != tc.legacySeen {
			t.Fatalf("%s: the recorded line-rule answer is stale (seen %v want %v)", tc.name, seen, tc.legacySeen)
		}
	}
}

// TestScanGoStatements_ClosuresAreIdentifiedByWhatTheyDo — the defect that mattered most. The
// replaced needle for "stranded reservation sweep" reduced to `go func() {`, which every
// anonymous goroutine begins with, so a new one inherited that entry's money-path reason.
func TestScanGoStatements_ClosuresAreIdentifiedByWhatTheyDo(t *testing.T) {
	const sweep = "\tgo func() {\n\t\tif n, err := dualToken.ReleaseStrandedReservations(ctx, d); err != nil {\n\t\t\t_ = n\n\t\t}\n\t}()\n"
	const other = "\tgo func() {\n\t\tlogger.Info(\"something else entirely\")\n\t}()\n"
	const commented = "\tgo func() { // publish detector staleness\n\t\tlogger.Info(\"something else entirely\")\n\t}()\n"

	for _, tc := range []struct {
		name   string
		body   string
		needle string
		want   bool
	}{
		{"the real sweep matches its own call", sweep, "dualToken.ReleaseStrandedReservations", true},
		{"an unrelated closure does NOT match it", other, "dualToken.ReleaseStrandedReservations", false},
		{"a closure carrying the detector COMMENT does not match the detector", commented, "patternDetectorHealth.PublishAge", false},
		{"a named call matches its callee", "\tgo batchRouter.StartPoller(ctx)\n", "batchRouter.StartPoller", true},
		{"a named call does not match another callee", "\tgo batchRouter.StartPoller(ctx)\n", "cpSyncer.Run", false},
	} {
		sites, err := scanGoStatements("synthetic.go", []byte(wrapMain(tc.body)))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(sites) != 1 {
			t.Fatalf("%s: sites=%d want 1", tc.name, len(sites))
		}
		// ⚠ A CLOSURE CARRIES NO CALLEE, BY DESIGN, and that is asserted here because nothing
		// else observed it: a control that gave func literals a rendered callee changed no
		// verdict. The callee is what the census NAMES an unclassified goroutine by, so without
		// this the failure message could degrade to `func() {…}` — the same for every closure,
		// which is how the replaced rule confused them in the first place.
		isClosure := strings.HasPrefix(strings.TrimSpace(tc.body), "go func(")
		if isClosure && sites[0].callee != "" {
			t.Errorf("%s: a func literal must carry no callee, got %q", tc.name, sites[0].callee)
		}
		if isClosure && len(sites[0].calls) == 0 {
			t.Errorf("%s: a func literal must be identified by the calls in its body, got none", tc.name)
		}
		if !isClosure && sites[0].callee == "" {
			t.Errorf("%s: a named call must carry its callee", tc.name)
		}
		if got := sites[0].matches(tc.needle); got != tc.want {
			t.Errorf("%s: matches(%q)=%v want %v (callee %q, calls %v)",
				tc.name, tc.needle, got, tc.want, sites[0].callee, sites[0].calls)
		}
	}
}

// TestScanGoStatements_ParseErrorsAreReturned — a scan that enumerates no goroutines reports
// every goroutine as classified.
func TestScanGoStatements_ParseErrorsAreReturned(t *testing.T) {
	sites, err := scanGoStatements("broken.go", []byte("package main\nfunc run() { this is not go\n"))
	if err == nil {
		t.Fatalf("unparseable source returned no error (sites=%v)", sites)
	}
	if sites != nil {
		t.Errorf("sites must be nil on a parse error, got %v", sites)
	}
}
