package main

import (
	"strings"
	"testing"
)

// admin_route_scan_table_test.go — the red-first table for scanAdminRoutes.
//
// `wantPaths` / `wantGated` are what is TRUE of the source; `linePaths` / `lineGated` are
// what the regex + three-line-window rule this replaced answered. Rows where they disagree
// are the defect; rows where they agree must keep passing on BOTH sides.

// legacyPathsAndGate is the replaced rule verbatim: every /v1/admin literal the regex finds,
// and "gated" = the gate name appearing anywhere in a three-line window from the path's line.
func legacyPathsAndGate(src, path string) (found, gated bool) {
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		for _, m := range adminPathRe.FindAllStringSubmatch(line, -1) {
			if m[1] != path {
				continue
			}
			found = true
			window := strings.Join(lines[i:min(i+3, len(lines))], "\n")
			if strings.Contains(window, operatorReadGate+"(") {
				gated = true
			}
		}
	}
	return found, gated
}

type adminRow struct {
	name      string
	body      string
	wantFound bool
	wantGated bool
	lineFound bool
	lineGated bool
}

const adminPath = "/v1/admin/lxc/grant"

var adminRows = []adminRow{
	{
		name:      "money route behind requireAdmin — correct wiring",
		body:      "\tauthed.Post(\"/v1/admin/lxc/grant\", requireAdmin(am, h))\n",
		wantFound: true, lineFound: true,
	},
	{
		name:      "money route widened to the operator read gate, one line",
		body:      "\tauthed.Post(\"/v1/admin/lxc/grant\", requireAdminOrOperatorRead(am, h))\n",
		wantFound: true, wantGated: true, lineFound: true, lineGated: true,
	},
	{
		name:      "the same widening, gate three lines below the path",
		body:      "\tauthed.Post(\n\t\t\"/v1/admin/lxc/grant\",\n\t\t// operator console rollout\n\t\t// see the brief\n\t\trequireAdminOrOperatorRead(am, h),\n\t)\n",
		wantFound: true, wantGated: true, lineFound: true,
	},
	{
		name:      "the same widening through a local alias of the gate",
		body:      "\tgate := requireAdminOrOperatorRead\n\tauthed.Post(\"/v1/admin/lxc/grant\", gate(am, h))\n",
		wantFound: true, wantGated: true, lineFound: true,
	},
	{
		name:      "correct wiring, the gate merely NAMED in a comment below the path",
		body:      "\tauthed.Post(\"/v1/admin/lxc/grant\", requireAdmin(am, h))\n\t// deliberately not requireAdminOrOperatorRead( — this route mints.\n",
		wantFound: true, lineFound: true, lineGated: true,
	},
	{
		name:      "the registration is COMMENTED OUT — the route does not exist",
		body:      "\t// authed.Post(\"/v1/admin/lxc/grant\", requireAdmin(am, h))\n\t_ = h\n",
		lineFound: true,
	},
	{
		// A SHARED LIMIT, kept as a row so it is recorded rather than discovered: a bare path
		// literal passed to ANY call reads as a registration to both rules. That is the
		// over-reporting direction — such a path lands in neither classification table and
		// TestEveryAdminRouteIsClassified fails loudly — so it is the safe one.
		name:      "a bare path literal passed to a non-router call — over-reported by both",
		body:      "\tlogger.Info(\"/v1/admin/lxc/grant\")\n",
		wantFound: true, lineFound: true,
	},
}

func TestScanAdminRoutes_ReadsRegistrationsAndGatesNotLines(t *testing.T) {
	for _, r := range adminRows {
		src := wrapMain(r.body)
		got, err := scanAdminRoutes("synthetic.go", []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		found, gated := false, false
		for _, x := range got {
			if x.path == adminPath {
				found = true
				if x.gated {
					gated = true
				}
			}
		}
		if found != r.wantFound || gated != r.wantGated {
			t.Errorf("%s: found=%v gated=%v want found=%v gated=%v", r.name, found, gated, r.wantFound, r.wantGated)
		}
		lf, lg := legacyPathsAndGate(src, adminPath)
		if lf != r.lineFound || lg != r.lineGated {
			t.Fatalf("%s: the recorded line-rule answer is stale (found %v want %v, gated %v want %v)",
				r.name, lf, r.lineFound, lg, r.lineGated)
		}
	}
}

// TestScanAdminRoutes_TheLineRuleWasWrongOnTheseRows pins how the replaced rule erred: it
// MISSED real widenings of a money route to the operator read credential, it FALSELY read a
// comment as a gate, and it counted a commented-out registration as a live route — which is
// what made the ghost check unable to see a route go.
func TestScanAdminRoutes_TheLineRuleWasWrongOnTheseRows(t *testing.T) {
	var missedGate, falseGate, phantomRoute, agree int
	for _, r := range adminRows {
		lf, lg := legacyPathsAndGate(wrapMain(r.body), adminPath)
		switch {
		case r.wantGated && !lg:
			missedGate++
		case !r.wantGated && lg:
			falseGate++
		case lf && !r.wantFound:
			phantomRoute++
		default:
			agree++
		}
	}
	if missedGate != 2 {
		t.Errorf("real widenings the line rule MISSED = %d, want 2", missedGate)
	}
	if falseGate != 1 {
		t.Errorf("comments the line rule read as a gate = %d, want 1", falseGate)
	}
	if phantomRoute != 1 {
		t.Errorf("commented-out registrations the line rule counted as routes = %d, want 1", phantomRoute)
	}
	if agree < 3 {
		t.Errorf("only %d rows describe behaviour the line rule got right — a table that is "+
			"red everywhere is not measuring the scanner", agree)
	}
}

// TestScanAdminRoutes_ExpandsTheLoopPrefixStructurally — the four loop-generated PAYOUT
// REVOCATION routes exist only as `"/v1/admin/held-mints/" + mt + "/adjudicate"`. A scanner
// that accepted only a bare literal would classify them as non-existent, which is the exact
// failure this file's own header warns about.
func TestScanAdminRoutes_ExpandsTheLoopPrefixStructurally(t *testing.T) {
	const body = "\tfor _, mt := range []string{\"a_mints\", \"b_mints\"} {\n" +
		"\t\tauthed.Post(\"/v1/admin/held-mints/\"+mt+\"/adjudicate\", requireAdmin(am, h))\n\t}\n"
	got, err := scanAdminRoutes("synthetic.go", []byte(wrapMain(body)))
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, r := range got {
		if r.path == "/v1/admin/held-mints/" {
			seen = true
			if r.gated {
				t.Error("the loop registration is not operator-read gated; the scan says it is")
			}
		}
	}
	if !seen {
		t.Error("the concatenated loop prefix was not enumerated — four payout-revocation routes would classify as non-existent")
	}
	mts, err := heldMintTypesFromAST("synthetic.go", []byte(wrapMain(body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(mts) != 2 || mts[0] != "a_mints" || mts[1] != "b_mints" {
		t.Errorf("mint types read from the loop = %v, want [a_mints b_mints]", mts)
	}

	// ⚠ IT MUST READ *THE* LOOP, NOT ANY LOOP WHOSE VALUE HAPPENS TO BE CALLED mt. Without
	// this the []string element-type check is asserted by nothing: a mutation removing it
	// changed no verdict, which makes it a rule that could be relaxed away with everything
	// green. Reading the four mint types out of some other loop is how the expansion silently
	// stops matching the routes the router actually registers.
	const otherLoop = "\tfor _, mt := range []any{\"a_mints\", \"b_mints\"} {\n\t\t_ = mt\n\t}\n"
	other, err := heldMintTypesFromAST("synthetic.go", []byte(wrapMain(otherLoop)))
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("mint types read from a non-[]string loop = %v, want none — the expansion must "+
			"come from the held-mints loop's own []string literal", other)
	}
}

// TestScanAdminRoutes_ParseErrorsAreReturned — a scan that enumerates nothing classifies
// nothing, and every "must not be gated" assertion passes for free.
func TestScanAdminRoutes_ParseErrorsAreReturned(t *testing.T) {
	got, err := scanAdminRoutes("broken.go", []byte("package main\n\nfunc run() { this is not go\n"))
	if err == nil {
		t.Fatalf("unparseable source returned no error (got %v)", got)
	}
	if got != nil {
		t.Errorf("routes must be nil on a parse error, got %v", got)
	}
	mts, err := heldMintTypesFromAST("broken.go", []byte("package main\n\nfunc run() { this is not go\n"))
	if err == nil {
		t.Fatalf("heldMintTypesFromAST returned no error (got %v)", mts)
	}
}
