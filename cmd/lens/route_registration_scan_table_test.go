package main

import (
	"regexp"
	"testing"
)

// route_registration_scan_table_test.go — the red-first table for scanRouteRegistrations.
//
// `wantBare` is what is TRUE of the source: /v1/economy/probe is bound on a router without
// going through the econ gate, so the master kill switch cannot withhold it. `lineBare` is
// what the regex this replaced answered.

// legacyBareReg is the replaced rule verbatim, kept as the thing under comparison.
var legacyBareReg = regexp.MustCompile(`\b(?:authed|pub|r)\.(?:Get|Post|Delete)\("([^"]+)"`)

func legacyBareEconomy(src string) bool {
	for _, m := range legacyBareReg.FindAllStringSubmatch(src, -1) {
		if isEconomyPath(m[1]) {
			return true
		}
	}
	return false
}

type regRow struct {
	name     string
	body     string
	wantBare bool
	lineBare bool
}

const probeHandler = "func(w http.ResponseWriter, _ *http.Request) {}"

var regRows = []regRow{
	{
		name:     "bare economy route, authed.Get, one line",
		body:     "\tauthed.Get(\"/v1/economy/probe\", " + probeHandler + ")\n",
		wantBare: true, lineBare: true,
	},
	{
		name:     "the same registration split across lines",
		body:     "\tauthed.Get(\n\t\t\"/v1/economy/probe\",\n\t\t" + probeHandler + ",\n\t)\n",
		wantBare: true,
	},
	{
		name:     "authed.Put — 10 real Put sites in main.go",
		body:     "\tauthed.Put(\"/v1/economy/probe\", " + probeHandler + ")\n",
		wantBare: true,
	},
	{
		name:     "authed.Patch — 2 real Patch sites",
		body:     "\tauthed.Patch(\"/v1/economy/probe\", " + probeHandler + ")\n",
		wantBare: true,
	},
	{
		name:     "r.Handle — 9 real Handle sites, and r is the UNAUTHENTICATED root router",
		body:     "\tr.Handle(\"/v1/economy/probe\", h)\n",
		wantBare: true,
	},
	{
		name:     "authed.Method — the path is the SECOND argument",
		body:     "\tauthed.Method(http.MethodGet, \"/v1/economy/probe\", h)\n",
		wantBare: true,
	},
	{
		name:     "behind a middleware chain — authed.With(mw).Get",
		body:     "\tauthed.With(mw).Get(\"/v1/economy/probe\", " + probeHandler + ")\n",
		wantBare: true, lineBare: false,
	},
	{
		name: "registered THROUGH the econ gate — correct, and the kill switch withholds it",
		body: "\tecon.get(authed, \"/v1/economy/probe\", " + probeHandler + ")\n",
	},
	{
		name:     "a bare registration that is COMMENTED OUT — it binds nothing",
		body:     "\t// authed.Get(\"/v1/economy/probe\", h)\n\t_ = h\n",
		lineBare: true,
	},
	{
		name: "a deliberately-bare NON-economy route",
		body: "\tauthed.Get(\"/v1/admin/distill/preview\", " + probeHandler + ")\n",
	},
}

func TestScanRouteRegistrations_ReadsCallsNotLines(t *testing.T) {
	for _, r := range regRows {
		src := wrapMain(r.body)
		regs, _, _, err := scanRouteRegistrations("synthetic.go", []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		bare := false
		for _, rg := range regs {
			if isEconomyPath(rg.path) {
				bare = true
			}
		}
		if bare != r.wantBare {
			t.Errorf("%s: bare economy route=%v want %v (regs %v)", r.name, bare, r.wantBare, regs)
		}
		if lb := legacyBareEconomy(src); lb != r.lineBare {
			t.Fatalf("%s: the recorded regex answer is stale (got %v want %v)", r.name, lb, r.lineBare)
		}
	}
}

// TestScanRouteRegistrations_TheRegexWasWrongOnTheseRows pins how the replaced rule erred: it
// MISSED bare economy routes the master kill switch could not withhold, and it falsely accused a
// commented-out registration. Both counts are pinned so neither drifts silently.
func TestScanRouteRegistrations_TheRegexWasWrongOnTheseRows(t *testing.T) {
	var missed, falseAccusation, agree int
	for _, r := range regRows {
		lb := legacyBareEconomy(wrapMain(r.body))
		switch {
		case r.wantBare && !lb:
			missed++
		case !r.wantBare && lb:
			falseAccusation++
		default:
			agree++
		}
	}
	if missed != 6 {
		t.Errorf("bare economy routes the regex MISSED = %d, want 6", missed)
	}
	if falseAccusation != 1 {
		t.Errorf("commented-out registrations the regex falsely accused = %d, want 1", falseAccusation)
	}
	if agree < 3 {
		t.Errorf("only %d rows describe behaviour the regex got right — a table that is red "+
			"everywhere is not measuring the scanner", agree)
	}
}

// TestScanRouteRegistrations_ReportsTheRouterAndTheUnknownMethods — the two halves the coverage
// guard is built on, asserted directly so neither can be removed without a red.
func TestScanRouteRegistrations_ReportsTheRouterAndTheUnknownMethods(t *testing.T) {
	const body = "\tauthed.With(mw).Get(\"/v1/economy/probe\", h)\n\tauthed.Teleport(\"/v1/economy/other\", h)\n"
	regs, routers, unknown, err := scanRouteRegistrations("synthetic.go", []byte(wrapMain(body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1 || regs[0].receiver != "authed.With(mw)" || regs[0].verb != "Get" {
		t.Errorf("registrations = %v, want one authed.With(mw).Get", regs)
	}
	if len(routers) != 1 || routers[0] != "authed" {
		t.Errorf("routers = %v, want [authed] — a middleware chain is the same router", routers)
	}
	if _, ok := unknown["Teleport"]; !ok {
		t.Errorf("unknown router methods = %v, want Teleport reported — an unrecognised method "+
			"that BINDS A PATH is exactly how an economy route becomes invisible", unknown)
	}
	if _, ok := unknown["Get"]; ok {
		t.Error("a known registration verb must not be reported as unknown")
	}
	if _, ok := unknown["With"]; ok {
		t.Error("a known non-registering method must not be reported as unknown")
	}
}

// TestScanRouteRegistrations_ParseErrorsAreReturned — a scan that finds no registrations
// reports no bare economy routes, which is a tripwire that cannot fire.
func TestScanRouteRegistrations_ParseErrorsAreReturned(t *testing.T) {
	regs, routers, unknown, err := scanRouteRegistrations("broken.go", []byte("package main\n\nfunc run() { this is not go\n"))
	if err == nil {
		t.Fatalf("unparseable source returned no error (regs=%v)", regs)
	}
	if regs != nil || routers != nil || unknown != nil {
		t.Errorf("all results must be nil on a parse error, got %v %v %v", regs, routers, unknown)
	}
}
