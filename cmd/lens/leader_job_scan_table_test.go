package main

import (
	"strings"
	"testing"
)

// leader_job_scan_table_test.go — the red-first table for scanLeaderJobs.
//
// Every row is a synthetic main.go. `wantWired` / `wantEconGated` are what is TRUE of
// the source; `lineWired` / `lineEconGated` are what the LINE rules these replaced
// answered. Rows where the two disagree are the defect; rows where they agree are the
// correct-behaviour rows that must keep passing on BOTH sides — a uniformly-red table
// would be measuring nothing.

// legacyFoundJob is TestAuditJobs_LeaderGated's rule verbatim, kept as the thing under
// comparison: a LINE containing both the selector and the quoted job name.
func legacyFoundJob(src, job string) bool {
	for _, ln := range strings.Split(src, "\n") {
		if strings.Contains(ln, "haComps.leader.Run") && strings.Contains(ln, `"`+job+`"`) {
			return true
		}
	}
	return false
}

// legacyEconGated is TestEconomyKillSwitch_WorkersGuarded's rule verbatim: find that
// line, then look UP AT MOST FOUR LINES for the literal gate text.
func legacyEconGated(src, job string) bool {
	lines := strings.Split(src, "\n")
	idx := -1
	for i, ln := range lines {
		if strings.Contains(ln, "haComps.leader.Run") && strings.Contains(ln, `"`+job+`"`) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	for j := idx; j >= 0 && j > idx-4; j-- {
		if strings.Contains(lines[j], "if cfg.EconomyEnabled {") {
			return true
		}
	}
	return false
}

func wrapMain(body string) string {
	return "package main\n\nfunc run() {\n" + body + "}\n"
}

type leaderRow struct {
	name          string
	body          string
	wantWired     bool // the job is really registered through leader.Run
	wantEconGated bool // …and really inside `if cfg.EconomyEnabled`
	lineWired     bool // what the line rule said
	lineEconGated bool
}

var leaderRows = []leaderRow{
	{
		name:          "registered and econ-gated (the shipped shape)",
		body:          "\tif cfg.EconomyEnabled {\n\t\tgo haComps.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t\t\ts.Start(lctx)\n\t\t})\n\t}\n",
		wantWired:     true,
		wantEconGated: true,
		lineWired:     true,
		lineEconGated: true,
	},
	{
		name:      "registered, no gate at all",
		body:      "\tgo haComps.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t\ts.Start(lctx)\n\t})\n",
		wantWired: true,
		lineWired: true,
	},
	{
		name: "never registered at all",
		body: "\t_ = s\n",
	},
	{
		name:      "COMMENTED OUT — the ordinary way to disable a job; it does not run",
		body:      "\t// go haComps.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t// \ts.Start(lctx)\n\t// })\n\t_ = s\n",
		lineWired: true,
	},
	{
		name:      "only a doc comment names the job",
		body:      "\t// \"w\" is started by haComps.leader.Run(ctx, \"w\", …) at boot.\n\t_ = s\n",
		lineWired: true,
	},
	{
		name:      "the tokens live only in a map literal",
		body:      "\tlabels := map[string]string{\"haComps.leader.Run\": \"w\"}\n\t_ = labels\n",
		lineWired: true,
	},
	{
		name:      "the tokens live only inside a log string",
		body:      "\tlogger.Debug(`haComps.leader.Run will start \"w\" once elected`)\n",
		lineWired: true,
	},
	{
		name:      "runs UNGATED as a plain goroutine — no leader election, every replica runs it",
		body:      "\t// leader gating via haComps.leader.Run(ctx, \"w\", …) was removed here.\n\tgo s.Start(ctx)\n",
		lineWired: true,
	},
	{
		name:          "ungated, with the econ gate surviving only as a comment",
		body:          "\t// if cfg.EconomyEnabled {\n\tgo haComps.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t\ts.Start(lctx)\n\t})\n",
		wantWired:     true,
		lineWired:     true,
		lineEconGated: true,
	},
	{
		name:          "ungated, with the econ gate quoted inside a log string",
		body:          "\tlogger.Debug(\"if cfg.EconomyEnabled { was checked at boot\")\n\tgo haComps.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t\ts.Start(lctx)\n\t})\n",
		wantWired:     true,
		lineWired:     true,
		lineEconGated: true,
	},
	{
		name:      "gated on a DIFFERENT flag, with a real econ block three lines above",
		body:      "\tif cfg.EconomyEnabled {\n\t\t_ = cfg.EconomyEnabled\n\t}\n\tif cfg.PoolRoyaltyMintingEnabled {\n\t\tgo haComps.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t\t\ts.Start(lctx)\n\t\t})\n\t}\n",
		wantWired: true,
		lineWired: true,
	},
	{
		name:          "econ-gated but nested far below the gate — more than four lines away",
		body:          "\tif cfg.EconomyEnabled {\n\t\t_ = s\n\t\t_ = d\n\t\t_ = ctx\n\t\t_ = lctxUnused\n\t\tgo haComps.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t\t\ts.Start(lctx)\n\t\t})\n\t}\n",
		wantWired:     true,
		wantEconGated: true,
		lineWired:     true,
	},
	{
		name:          "registered in the ELSE branch of the econ gate — it runs when the economy is OFF",
		body:          "\tif cfg.EconomyEnabled {\n\t\t_ = s\n\t} else {\n\t\tgo haComps.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t\t\ts.Start(lctx)\n\t\t})\n\t}\n",
		wantWired:     true,
		lineWired:     true,
		lineEconGated: true,
	},
	{
		name:          "registration split over two lines — invisible to any line rule",
		body:          "\tif cfg.EconomyEnabled {\n\t\tgo haComps.leader.Run(ctx,\n\t\t\t\"w\", d, func(lctx context.Context) { s.Start(lctx) })\n\t}\n",
		wantWired:     true,
		wantEconGated: true,
	},
	{
		name:      "leader.Run called with fewer than two arguments — nothing to name",
		body:      "\tgo haComps.leader.Run(ctx)\n",
		lineWired: false,
	},
	{
		name:      "the job name is a variable, not a literal — the name is unknowable here",
		body:      "\tgo haComps.leader.Run(ctx, jobName, d, func(lctx context.Context) {\n\t\ts.Start(lctx)\n\t})\n",
		lineWired: false,
	},
	{
		name:      "a DIFFERENT leader's Run — not this repo's wiring seam",
		body:      "\tgo other.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t\ts.Start(lctx)\n\t})\n",
		lineWired: false,
	},
	{
		name:          "the gate is a CLOSED block above the call, not around it",
		body:          "\tif cfg.EconomyEnabled {\n\t\t_ = s\n\t}\n\tgo haComps.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t\ts.Start(lctx)\n\t})\n",
		wantWired:     true,
		lineWired:     true,
		lineEconGated: true,
	},
}

func rowJobs(t *testing.T, r leaderRow) []leaderJob {
	t.Helper()
	jobs, err := scanLeaderJobs("synthetic.go", []byte(wrapMain(r.body)))
	if err != nil {
		t.Fatalf("%s: scanLeaderJobs: %v", r.name, err)
	}
	return jobs
}

// TestScanLeaderJobs_ReadsCallsAndGatesNotLines is the red-first table: the AST scanner
// must agree with the TRUTH column on every row.
func TestScanLeaderJobs_ReadsCallsAndGatesNotLines(t *testing.T) {
	for _, r := range leaderRows {
		jobs := rowJobs(t, r)
		var found *leaderJob
		for i := range jobs {
			if jobs[i].name == "w" {
				found = &jobs[i]
				break
			}
		}
		if (found != nil) != r.wantWired {
			t.Errorf("%s: wired=%v want %v", r.name, found != nil, r.wantWired)
			continue
		}
		gated := found != nil && found.gatedOn("cfg.EconomyEnabled")
		if gated != r.wantEconGated {
			t.Errorf("%s: econ-gated=%v want %v (conds %v)", r.name, gated, r.wantEconGated, jobs)
		}
	}
}

// TestScanLeaderJobs_TheLineRulesWereWrongOnTheseRows pins WHICH rows the replaced line
// rules answered wrongly. If a future change reintroduces a line rule this count moves,
// and if the table were uniformly red it would not be measuring the scanners at all.
func TestScanLeaderJobs_TheLineRulesWereWrongOnTheseRows(t *testing.T) {
	var wiredWrong, gateWrong, agree int
	for _, r := range leaderRows {
		src := wrapMain(r.body)
		lw, lg := legacyFoundJob(src, "w"), legacyEconGated(src, "w")
		if lw != r.lineWired || lg != r.lineEconGated {
			t.Fatalf("%s: the recorded line-rule answer is stale (wired %v want %v, gated %v want %v)",
				r.name, lw, r.lineWired, lg, r.lineEconGated)
		}
		switch {
		case lw != r.wantWired:
			wiredWrong++
		case lg != r.wantEconGated:
			gateWrong++
		default:
			agree++
		}
	}
	if wiredWrong != 6 {
		t.Errorf("rows where the line rule mis-answered WIRED = %d, want 6", wiredWrong)
	}
	if gateWrong != 5 {
		t.Errorf("rows where the line rule mis-answered ECON-GATED = %d, want 5", gateWrong)
	}
	if agree < 5 {
		t.Errorf("only %d rows describe behaviour the line rule got RIGHT — a table that is "+
			"red everywhere is not measuring the scanner", agree)
	}
}

// TestScanLeaderJobs_ElseBranchIsRenderedAsANegation — the else arm of a gate is not the
// gate. Without this the `!(cond)` rendering is asserted by nothing (gatedOn would answer
// "not gated" either way), the rule could be relaxed away with everything green, and the
// failure message for an else-registered worker would print an empty condition list
// instead of naming the branch it is actually in.
func TestScanLeaderJobs_ElseBranchIsRenderedAsANegation(t *testing.T) {
	const body = "\tif cfg.EconomyEnabled {\n\t\t_ = s\n\t} else {\n\t\tgo haComps.leader.Run(ctx, \"w\", d, func(lctx context.Context) {\n\t\t\ts.Start(lctx)\n\t\t})\n\t}\n"
	jobs, err := scanLeaderJobs("synthetic.go", []byte(wrapMain(body)))
	if err != nil {
		t.Fatalf("scanLeaderJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	if got := jobs[0].conds; len(got) != 1 || got[0] != "!(cfg.EconomyEnabled)" {
		t.Errorf("else-branch conditions = %v, want [!(cfg.EconomyEnabled)]", got)
	}
	if jobs[0].gatedOn("cfg.EconomyEnabled") {
		t.Error("a worker in the ELSE branch runs when the economy is OFF — it must not read as gated")
	}
}

// TestScanLeaderJobs_ParseErrorsAreReturned — a scanner that answers "no jobs" because
// the file did not parse is a guard that cannot fail: every "must be gated" assertion
// downstream passes by finding nothing to complain about.
func TestScanLeaderJobs_ParseErrorsAreReturned(t *testing.T) {
	jobs, err := scanLeaderJobs("broken.go", []byte("package main\n\nfunc run() { this is not go\n"))
	if err == nil {
		t.Fatalf("unparseable source returned no error (jobs=%v)", jobs)
	}
	if jobs != nil {
		t.Errorf("jobs must be nil on a parse error, got %v", jobs)
	}
}

// TestCallsOutsideLeaderJob_ReadsContainmentNotText — the #526 half. The cache-warmer guard had a
// positive rule ("the leader job is registered") and a negative one ("the exact text
// `go cacheWarmer.Start(ctx` is absent"), and a CLOSURE walked past both: the job line left in a
// comment satisfied the first and `go func() { cacheWarmer.Start(ctx, …) }()` did not match the
// second. Containment answers what both were reaching for.
func TestCallsOutsideLeaderJob_ReadsContainmentNotText(t *testing.T) {
	const gated = "\tgo haComps.leader.Run(ctx, \"cache-warmer\", d, func(lctx context.Context) {\n" +
		"\t\tcacheWarmer.Start(lctx, h)\n\t})\n"
	for _, tc := range []struct {
		name  string
		body  string
		loose int
	}{
		{"inside the named leader job", gated, 0},
		{"a bare `go cacheWarmer.Start(ctx, …)`", "\tgo cacheWarmer.Start(ctx, h)\n", 1},
		{"inside a CLOSURE, not the leader job", "\tgo func() { cacheWarmer.Start(ctx, h) }()\n", 1},
		{"gated, PLUS a second ungated start", gated + "\tgo func() { cacheWarmer.Start(ctx, h) }()\n", 1},
		{
			name:  "inside a leader job with a DIFFERENT name",
			body:  "\tgo haComps.leader.Run(ctx, \"other-job\", d, func(lctx context.Context) {\n\t\tcacheWarmer.Start(lctx, h)\n\t})\n",
			loose: 1,
		},
		{"only a COMMENT calls it", "\t// go cacheWarmer.Start(ctx, h)\n\t_ = cacheWarmer\n", 0},
		{
			// ⚠ INSIDE SOMETHING THAT TAKES THE JOB NAME IS NOT INSIDE THE JOB. Without this row
			// the leader.Run check is asserted by nothing: a control that accepted ANY enclosing
			// call changed no verdict, because every other row's wrapper has no string argument.
			// A retry/telemetry wrapper taking a job name is an ordinary thing to write, and it
			// does not elect a leader.
			name:  "inside a wrapper that merely takes the job NAME",
			body:  "\tgo retryWrapper(ctx, \"cache-warmer\", func() { cacheWarmer.Start(ctx, h) })\n",
			loose: 1,
		},
		{"a DIFFERENT receiver's Start", "\tgo otherWarmer.Start(ctx, h)\n", 0},
		{"nested two levels inside the named job",
			"\tgo haComps.leader.Run(ctx, \"cache-warmer\", d, func(lctx context.Context) {\n" +
				"\t\tif ok {\n\t\t\tcacheWarmer.Start(lctx, h)\n\t\t}\n\t})\n", 0},
	} {
		got, err := callsOutsideLeaderJob("synthetic.go", []byte(wrapMain(tc.body)), "cacheWarmer", "Start", "cache-warmer")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got) != tc.loose {
			t.Errorf("%s: calls outside the job = %d %v, want %d", tc.name, len(got), got, tc.loose)
		}
	}
	if _, err := callsOutsideLeaderJob("broken.go", []byte("package main\nfunc run() { this is not go\n"),
		"cacheWarmer", "Start", "cache-warmer"); err == nil {
		t.Error("a parse error was swallowed — an unreadable file would report NO ungated calls, " +
			"which is the wrong direction to be wrong in")
	}
}
