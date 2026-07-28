package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
)

// THE GUARD THAT MAKES THE ZERO UNREACHABLE, not merely handled.
//
// alerts.CostUSD and alerts.CostUSDDetailed return 0 for a model the catalog does not know. That
// contract is correct for their purpose — they were written for ALERTING, where a missed alert beats a
// false one off bad data — and it is a revenue hole the moment the same function prices a bill. Eleven
// callers were fine; one was not, and the result was requests served free.
//
// Fixing the one caller does not stop the twelfth. So this test asserts the RULE structurally: the
// functions that compute a HOLD or a CHARGE must price through alerts.CostUSDResolved, which has no
// zero outcome, and must not call the zero-returning variants at all.
//
// This is the internal/royaltyhaircut/import_guard_test.go pattern — AST-parse the file, walk the
// calls, fail on a banned identifier — applied to money-path pricing instead of ledger reach.

// moneyPathFuncs are the functions whose result becomes a hold or a charge. Adding a new one? Add it
// here, or the guard silently stops covering it.
// ⚠ THIS LIST WAS TOO SHORT ONCE ALREADY, and the omission was a live free path. `lxcEstimate` was
// missing, so the guard passed while the immediate-debit fallback (agentAllocationBlocks, taken whenever
// reservations are inactive and default-ON) served unpriced traffic with NO DEBIT, and the balance gate
// never blocked it. The guard's name promised coverage it did not have.
//
// The rule for this map: if a function's result decides HOW MUCH is charged/held OR WHETHER a request is
// admitted on balance, it belongs here. "It's only an estimate" is not an exemption — an estimate that
// gates money is money.
var moneyPathFuncs = map[string]string{
	"reserveEstimateLXC":     "computes the pre-serve HOLD",
	"settleReservationBasis": "computes the delivered CHARGE",
	// resolveCacheReservation was SPLIT so the saving could be headered before the body goes out
	// (the serve precedes the settle). Both halves are money-path and both are listed: the pricer
	// decides HOW MUCH, the settler turns it into a debit. Listing only one would leave the other
	// exactly as uncovered as `lxcEstimate` used to be.
	"pricePooledServe":        "prices a pooled hit, discount applied",
	"settleReservationPooled": "turns that price into the consumer's DEBIT",
	"lxcEstimate":             "prices the immediate agent debit AND the balance admission gate",
}

// bannedInMoneyPath are the pricing helpers that turn an unknown model into a spendable zero.
var bannedInMoneyPath = map[string]bool{"CostUSD": true, "CostUSDDetailed": true}

func TestMoneyPathNeverPricesWithAZeroReturningHelper(t *testing.T) {
	// os.ReadDir + ParseFile, not parser.ParseDir (deprecated since Go 1.25) — the same shape
	// internal/royaltyhaircut/import_guard_test.go uses.
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("parsed no non-test files — the guard would pass vacuously")
	}

	var offenders []string
	seen := map[string]bool{}
	{
		for _, file := range files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Name == nil {
					return true
				}
				why, watched := moneyPathFuncs[fn.Name.Name]
				if !watched {
					return true
				}
				seen[fn.Name.Name] = true
				ast.Inspect(fn, func(inner ast.Node) bool {
					sel, ok := inner.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					ident, ok := sel.X.(*ast.Ident)
					if !ok || ident.Name != "alerts" || !bannedInMoneyPath[sel.Sel.Name] {
						return true
					}
					offenders = append(offenders, fn.Name.Name+" ("+why+") calls alerts."+sel.Sel.Name+
						" at "+fset.Position(sel.Pos()).String()+" — that returns 0 for an unknown model, so this "+
						"would serve unpriced traffic free. Use alerts.CostUSDResolved, which has no zero outcome.")
					return true
				})
				return true
			})
		}
	}

	// POSITIVE CONTROL: the walker must actually have found the functions it claims to guard. A guard
	// that silently stops matching (a rename, a moved file) is indistinguishable from a passing one —
	// the exact failure mode this whole change is about.
	for name := range moneyPathFuncs {
		if !seen[name] {
			t.Errorf("guard did not find %q in this package — it was renamed or moved, and the guard is "+
				"no longer covering it. Update moneyPathFuncs.", name)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("money-path pricing uses a zero-returning helper:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// And prove the ban is a real ban: the banned names must genuinely be the zero-returning ones, so the
// guard is pointed at the right target rather than at a name that no longer means anything.
func TestBannedHelpersReallyReturnZeroForUnknownModels(t *testing.T) {
	if got := alerts.CostUSD(unknownModel, 1_000_000, 1_000_000); got != 0 {
		t.Fatalf("alerts.CostUSD on an unknown model = %v, want 0 — the guard bans it for returning zero, "+
			"and if that is no longer true the guard's premise has changed", got)
	}
}
