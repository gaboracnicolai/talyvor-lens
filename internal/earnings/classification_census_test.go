package earnings

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// classification_census_test.go — the STATIC half of this package's completeness guard.
//
// A hand-written list of ledger types is exactly the shape that goes stale: W6.10 measured the
// sibling case in this repo, where cmd/lens's compose-forwarding guard was green over a variable
// that was absent from docker-compose.yaml entirely, because its `mustForward` map is maintained by
// the same person who would have remembered the thing it guards. So this census DERIVES the
// population from source instead of restating it.
//
// ⚠ WHAT THIS CENSUS CANNOT SEE, SAID HERE RATHER THAN DISCOVERED LATER. It reads packages that
// declare their ledger types as exported Go CONSTANTS: internal/mining and internal/povi. It is
// structurally blind to internal/economy, which writes five ledger types as BARE STRING LITERALS
// (marketplace_buy, marketplace_fee, marketplace_unsold_refund, stake, unstake) with no constant to
// find. Those five are classified by hand in classify.go and the RUNTIME Unclassified bucket is
// what catches a sixth. Two guards, and neither one is the whole answer.

var typeConstRE = struct{ prefix string }{prefix: "Type"}

// declaredLedgerTypes parses a package directory for `TypeX = "literal"` const declarations and
// returns literal -> Go constant name.
func declaredLedgerTypes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("[CENSUS-READ] cannot read %s, so this census asserted nothing: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("[CENSUS-PARSE] %s: %v", name, err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if !strings.HasPrefix(id.Name, typeConstRE.prefix) || !id.IsExported() || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil || v == "" {
						continue
					}
					out[v] = id.Name
				}
			}
		}
	}
	return out
}

func TestE1_EveryConstantDeclaredLedgerTypeIsClassified(t *testing.T) {
	population := map[string]string{}
	for _, dir := range []string{filepath.Join("..", "mining"), filepath.Join("..", "povi")} {
		for lit, name := range declaredLedgerTypes(t, dir) {
			population[lit] = name
		}
	}

	// FLOORS, both directions. A census with no floor passes loudest when it is reading nothing.
	if len(population) < 30 {
		t.Fatalf("[E1-FLOOR] the census found only %d declared ledger types — internal/mining alone declares far more, so this is reading the wrong files and 'all classified' would be meaningless", len(population))
	}
	for _, anchor := range []string{"pool_royalty", "cache_mine", "pool_royalty_held", "receipt_mine_provisional"} {
		if _, ok := population[anchor]; !ok {
			t.Fatalf("[E1-ANCHOR] the census did not find %q, which is declared as a constant and MUST be in any correct population. If the anchor is invisible to this parser, so is a new type.", anchor)
		}
	}

	var unclassified []string
	for lit, constName := range population {
		if Classify(lit).Class == Unclassified {
			unclassified = append(unclassified, lit+" ("+constName+")")
		}
	}
	sort.Strings(unclassified)
	for _, u := range unclassified {
		t.Errorf("[E1] ledger type %s is declared as a constant and internal/earnings does not classify it. "+
			"A new mint type that nobody classifies is silently worth ZERO on a customer's earnings screen. "+
			"Add it to classify.go's rules WITH a reason — including 'this is not income, because …'.", u)
	}
}

// TestE2_TheHandClassifiedLiteralsAreStillTheOnlyOnes pins the boundary the census above cannot
// cross. It fails when internal/economy grows a SIXTH bare-literal ledger type, which is the exact
// gap the static census is blind to — so the blindness is at least bounded and dated.
func TestE2_TheHandClassifiedLiteralsAreStillTheOnlyOnes(t *testing.T) {
	dir := filepath.Join("..", "economy")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("[E2-READ] cannot read %s, so this bound asserted nothing: %v", dir, err)
	}

	// Derive the literals rather than restate them: every string argument passed to a ledger
	// credit/debit in internal/economy is a ledger type this package must know about.
	fset := token.NewFileSet()
	found := map[string]string{}
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("[E2-PARSE] %s: %v", name, err)
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isLedgerMove(sel.Sel.Name) {
				return true
			}
			for _, a := range call.Args {
				lit, ok := a.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil || !looksLikeLedgerType(v) {
					continue
				}
				found[v] = name
			}
			return true
		})
	}

	// FLOORS. Without them this test passes hardest when the parser matched nothing.
	if files < 3 {
		t.Fatalf("[E2-FLOOR] only %d non-test files parsed in %s — wrong directory", files, dir)
	}
	if len(found) < 5 {
		t.Fatalf("[E2-FLOOR] the walk found %d ledger-type literals (%v); internal/economy is known to write at least 5, so this is not reading the call sites", len(found), found)
	}

	for lit, file := range found {
		if Classify(lit).Class == Unclassified {
			t.Errorf("[E2] internal/economy/%s writes ledger type %q as a BARE STRING LITERAL and internal/earnings does not classify it. "+
				"No constant exists for it, so the static census in TestE1 is structurally blind here — this test is the only thing between that literal and a silent zero.", file, lit)
		}
	}
}

// isLedgerMove names the calls that write a lens_token_ledger row. Kept narrow deliberately: a wide
// match would sweep in unrelated string arguments and bury the five that matter.
func isLedgerMove(fn string) bool {
	switch fn {
	case "CreditTx", "DebitTx", "Credit", "Debit", "CreditHeldTx":
		return true
	}
	return false
}

// looksLikeLedgerType filters the string arguments down to lowercase_snake tokens, which is the
// shape every ledger type in this repo has.
func looksLikeLedgerType(v string) bool {
	if len(v) < 4 || len(v) > 40 {
		return false
	}
	for _, r := range v {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

// TestE3_EveryRuleCarriesItsReason — a classification without an argument is a verdict, and the next
// person to add a mint type needs the argument to decide where theirs goes.
func TestE3_EveryRuleCarriesItsReason(t *testing.T) {
	if len(rules) < 35 {
		t.Fatalf("[E3-FLOOR] only %d rules — this test would pass over a nearly empty map", len(rules))
	}
	for lit, r := range rules {
		if len(strings.TrimSpace(r.Reason)) < 20 {
			t.Errorf("[E3] rule %q has no usable reason (%q)", lit, r.Reason)
		}
		if r.Class == Settled && r.Kind == NotIncome {
			t.Errorf("[E3-KIND] rule %q is settled income with kind not_income — one of the two is wrong", lit)
		}
		if r.Class == NotEarnings && r.Kind != NotIncome {
			t.Errorf("[E3-KIND] rule %q is not_earnings but claims kind %q", lit, r.Kind)
		}
	}
}

// TestE4_StakeYieldIsSettledButNeverContribution is the one classification worth its own test,
// because getting it wrong makes a SENTENCE false rather than a number wrong. "Your answers earned
// $6 back" must not include yield on locked capital — nobody wrote an answer for that.
func TestE4_StakeYieldIsSettledButNeverContribution(t *testing.T) {
	r := Classify("stake_yield")
	if r.Class != Settled {
		t.Fatalf("[E4] stake_yield is in counted supply and credited to the workspace, so it IS settled income; got class %q", r.Class)
	}
	if r.Kind != Capital {
		t.Fatalf("[E4-KIND] stake_yield is classified %q. If it enters the CONTRIBUTION subtotal, the sentence 'your answers earned this' becomes false while the total stays right — the worst kind of wrong number.", r.Kind)
	}
	// and the control: the headline contribution type must NOT be capital, or the distinction above
	// is satisfied by classifying everything as capital.
	if got := Classify("pool_royalty"); got.Kind != Contribution || got.Class != Settled {
		t.Fatalf("[E4-CONTROL] pool_royalty must be settled contribution; got class=%q kind=%q", got.Class, got.Kind)
	}
}
