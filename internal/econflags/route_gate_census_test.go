package econflags

// RULE D — THE MONEY READOUT MUST ACCOUNT FOR EVERY FLAG THAT DECIDES WHETHER A
// SURFACE EXISTS AT ALL.
//
// WHY THIS FILE EXISTS: RULES A, B AND C ARE STRUCTURALLY INCAPABLE OF SEEING A
// ROUTE GATE, AND ONE OF THEM WAS THE FIAT INTAKE PATH.
//
// forceoff_transcription_test.go says of rule C, in its own words: "THIS IS A
// NAME-SHAPE RULE AND THEREFORE A FLOOR, NOT A CENSUS... there is no such
// declaration for 'money-path flag', so this one is keyed on the name — the same
// weak instrument that let formatterReach watch a defect happen one repo over."
// That is exactly right, and this file is the rule it asked for. It is keyed on
// what the code DECLARES, not on what a field is called:
//
//	cmd/lens declares a gate type per surface — `type billReg struct{ on bool }`,
//	econReg, dashReg, batchReg — whose entire purpose is "off ⇒ the route is never
//	registered ⇒ chi-native 404". A config bool that constructs one of those is a
//	flag that decides whether a whole surface exists. That is a boundary a human
//	already drew in the source, the same property rules A and B lean on, and it is
//	visible to an AST walk.
//
// WHAT IT CAUGHT, MEASURED BEFORE IT WAS FIXED (this test was written red):
//
//	BillingEnabled — the U18b FIAT surface: POST /v1/billing/webhook (the Stripe
//	  callback that CREDITS a paid purchase), POST /v1/workspaces/{wsID}/billing/
//	  checkout (where the customer is CHARGED) and GET /v1/admin/billing/purchases.
//	  It is invisible to rules A and B by DESIGN — config.go deliberately keeps it
//	  out of the force-off block because fiat billing must run with the economy off
//	  — and invisible to rule C because "Billing" contains neither "Mint" nor "LXC".
//	  So the readout that exists to answer "is a money path armed?" was silent about
//	  whether the process could take money at all, and no rule could have said so.
//	  billing_routes.go states the consequence itself: an 'anomalous' row means the
//	  customer was CHARGED and NOT credited.
//	BatchEnabled — the lane batch_routes.go calls "the unbilled batch lane", whose
//	  gate exists because a completed job "would debit nothing while Talyvor pays
//	  the provider".
//
// THE RULE FOR THE EXEMPTION MAP, and it is the whole taxonomy this file commits
// to: a surface gate is EXEMPT only when opening or closing it cannot change
// whether value moves — no charge, no hold, no credit, no mint, no balance
// decision. "It is only a read surface" is the exemption; "it is only an
// estimate" is not (internal/proxy/money_path_pricing_guard_test.go learned that
// one the expensive way). An exemption must say why in a sentence an operator
// could disagree with. Silence is not available: a gate that is neither reported
// nor exempted reds.
//
// WHY AN EXEMPTION MAP RATHER THAN A CLEVERER PREDICATE. A predicate that decided
// "money" automatically would be another instrument keyed on a shape, and this
// package has now been bitten by two of those. A gate added tomorrow reds this
// test until a human classifies it in writing — the classification is the review,
// the same way TestReportedFlagSetIsPinned makes a removal a diff.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// cmdLensDir is parsed rather than imported: package main cannot be imported, and
// the gates live there because that is where routing is wired.
const cmdLensDir = "../../cmd/lens"

// routeGateExempt lists surface gates that move no value, each with the reason.
// Read the file doc comment before adding one.
var routeGateExempt = map[string]string{
	"DashboardEnabled": "gates GET /dashboard, an HTML read surface. It renders numbers the " +
		"money paths produced; it neither charges, holds, credits, mints nor admits a request " +
		"on balance. Turning it off hides a view and moves nothing.",
}

// FLOORS. Every assertion below is a walk over a parsed set, and a walk over an
// empty set passes. These are what stands between "every surface gate is
// accounted for" and "my parser matched nothing and reported a clean product".
// Measured at the commit that added this file: 4 gate types, 4 gate-controlling
// config bools. Set below today's numbers so an ordinary addition does not red
// them, far enough above zero that a parse returning nothing does.
const (
	floorGateTypes = 3
	floorGateFlags = 3
)

// parseCmdLens parses every non-test .go file in cmd/lens.
func parseCmdLens(t *testing.T) []*ast.File {
	t.Helper()
	ents, err := os.ReadDir(cmdLensDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", cmdLensDir, err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(cmdLensDir, n), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		out = append(out, f)
	}
	return out
}

// gateTypes returns the route-registration gate types: `type X struct{ on bool }`.
//
// The shape, not the name, is the key. A gate called `stripeSwitch` would be found;
// a struct called `fooReg` carrying different fields would not.
func gateTypes(files []*ast.File) map[string]bool {
	gates := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil || len(st.Fields.List) != 1 {
				return true
			}
			fld := st.Fields.List[0]
			id, ok := fld.Type.(*ast.Ident)
			if !ok || id.Name != "bool" || len(fld.Names) != 1 || fld.Names[0].Name != "on" {
				return true
			}
			gates[ts.Name.Name] = true
			return true
		})
	}
	return gates
}

// gateControllingFlags returns every config bool that flows into constructing a
// gate, mapped to the construction that consumed it.
//
// Both shapes the tree uses are covered, because covering only one is how a census
// silently shrinks: the composite literal (`billReg{on: cfg.BillingEnabled}`) and
// the constructor (`newBatchReg(cfg.BatchEnabled, false)`), which exists because
// that gate refuses to open on the flag alone.
func gateControllingFlags(files []*ast.File, gates map[string]bool) map[string]string {
	ctorOf := func(name string) bool {
		if !strings.HasPrefix(name, "new") || len(name) < 4 {
			return false
		}
		return gates[strings.ToLower(name[3:4])+name[4:]]
	}
	found := map[string]string{}
	record := func(e ast.Expr, via string) {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			e = kv.Value
		}
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok || x.Name != "cfg" {
			return
		}
		found[sel.Sel.Name] = via
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				if id, ok := v.Type.(*ast.Ident); ok && gates[id.Name] {
					for _, el := range v.Elts {
						record(el, id.Name+"{on: ...}")
					}
				}
			case *ast.CallExpr:
				if id, ok := v.Fun.(*ast.Ident); ok && ctorOf(id.Name) {
					for _, a := range v.Args {
						record(a, id.Name+"(...)")
					}
				}
			}
			return true
		})
	}
	return found
}

// TestEverySurfaceGateFlagIsReportedOrExempted is rule D.
func TestEverySurfaceGateFlagIsReportedOrExempted(t *testing.T) {
	files := parseCmdLens(t)

	gates := gateTypes(files)
	if len(gates) < floorGateTypes {
		t.Fatalf("found only %d route-registration gate types in %s (floor %d) — the type walk "+
			"matched nothing and every check below would pass vacuously",
			len(gates), cmdLensDir, floorGateTypes)
	}

	flags := gateControllingFlags(files, gates)
	if len(flags) < floorGateFlags {
		t.Fatalf("found only %d config bools constructing a gate (floor %d), from gate types %v — "+
			"the construction walk matched nothing and this rule would pass over any omission",
			len(flags), floorGateFlags, sortedKeys(gates))
	}

	reported := reportedNames(t)
	names := make([]string, 0, len(flags))
	for n := range flags {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		if reported[name] {
			if why, exempt := routeGateExempt[name]; exempt {
				t.Errorf("config.Config.%s is BOTH reported by econflags and listed in "+
					"routeGateExempt (%q) — the exemption says this gate moves no value while "+
					"the readout presents it as a money flag; one of the two is wrong and a "+
					"reader cannot tell which", name, why)
			}
			continue
		}
		if _, exempt := routeGateExempt[name]; exempt {
			continue
		}
		t.Errorf("config.Config.%s decides whether a whole surface is registered (%s) and "+
			"econflags neither reports it nor exempts it — rules A/B see only config.go's "+
			"force-off block and rule C only Mint-or-LXC names, so NOTHING else in this package "+
			"can see a surface gate. Report it, or add it to routeGateExempt with a reason that "+
			"says why opening or closing it cannot move value",
			name, flags[name])
	}
}

// A stale exemption is its own defect: it silences rule D for a flag that may no
// longer gate anything, and it reads as a considered decision. Measured against
// the same parse, so it cannot drift.
func TestNoStaleRouteGateExemption(t *testing.T) {
	files := parseCmdLens(t)
	gates := gateTypes(files)
	if len(gates) < floorGateTypes {
		t.Fatalf("found only %d gate types (floor %d) — refusing a vacuous comparison",
			len(gates), floorGateTypes)
	}
	flags := gateControllingFlags(files, gates)
	if len(flags) < floorGateFlags {
		t.Fatalf("found only %d gate-controlling flags (floor %d) — refusing a vacuous comparison",
			len(flags), floorGateFlags)
	}
	for name, why := range routeGateExempt {
		if _, ok := flags[name]; !ok {
			t.Errorf("routeGateExempt lists %q (%q) and no gate in %s is constructed from it — "+
				"the exemption is stale and is now silencing nothing while looking like a "+
				"considered decision", name, why, cmdLensDir)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("routeGateExempt[%q] carries no reason — an exemption an operator cannot "+
				"disagree with is indistinguishable from an omission", name)
		}
	}
}
