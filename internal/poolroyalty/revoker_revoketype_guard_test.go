package poolroyalty

// revoker_revoketype_guard_test.go — the wiring + money-safety guard for the CLAWBACK ledger label.
//
// revokeTypeForTable is the ONLY place a claim table becomes a clawback ledger type. A new held
// mint wired to a Revoker without a map entry would be fail-closed at revoke (revokeOne refuses) —
// safe, but it strands clawbacks until someone notices. This guard moves that discovery to CI: it
// reads the real cmd/lens/main.go and pins every table actually wired to a Revoker against the map.
//
// It is the sibling of sweeper_finaltype_guard_test.go, with the money invariant INVERTED: a
// finalize type MUST be counted in GetTotalSupply (settlement mints supply); a revoke type must be
// counted in NEITHER GetTotalSupply NOR the burned list (a clawback of held is supply-neutral and
// is not a burn). The guards below pin that inversion so the tempting wrong fix — giving a clawback
// its own type but adding it to a total's allow-list — fails here.
//
// Source-parsing uses go/ast (not a regexp) because the four P-o-I tables are wired through a
// `for _, mt := range []string{...}` loop, whose table literals a per-call regexp cannot see.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/mining"
)

// wiredRevokerTables reads cmd/lens/main.go and returns every claim table handed to a Revoker,
// across all three wiring shapes:
//
//	poolroyalty.NewRevoker(pool, tokenLedger)                          → pool_royalty_mints (implicit)
//	poolroyalty.NewRevokerForTable(pool, tokenLedger, "distill_...")   → the string-literal table
//	for _, mt := range []string{...} { NewRevokerForTable(.., mt) }    → each table in the slice
func wiredRevokerTables(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "../../cmd/lens/main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	wired := map[string]bool{}

	isRevokerCtor := func(call *ast.CallExpr, name string) bool {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == name
	}

	// (1) direct calls: bare NewRevoker (implicit pool_royalty_mints) + NewRevokerForTable("literal").
	ast.Inspect(af, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch {
		case isRevokerCtor(call, "NewRevoker"):
			wired["pool_royalty_mints"] = true
		case isRevokerCtor(call, "NewRevokerForTable"):
			if len(call.Args) >= 3 {
				if bl, ok := call.Args[2].(*ast.BasicLit); ok && bl.Kind == token.STRING {
					wired[strings.Trim(bl.Value, `"`)] = true
				}
			}
		}
		return true
	})

	// (2) loop-driven wiring: a `for _, mt := range []string{...}` whose body wires a Revoker with
	// the range variable wires every table in the slice. Captured precisely (only when the body
	// actually uses the loop var in a NewRevokerForTable call) so unrelated []string{} don't leak in.
	ast.Inspect(af, func(n ast.Node) bool {
		rng, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		valID, ok := rng.Value.(*ast.Ident)
		if !ok {
			return true
		}
		bodyWires := false
		ast.Inspect(rng.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok || !isRevokerCtor(call, "NewRevokerForTable") {
				return true
			}
			for _, a := range call.Args {
				if id, ok := a.(*ast.Ident); ok && id.Name == valID.Name {
					bodyWires = true
				}
			}
			return true
		})
		if !bodyWires {
			return true
		}
		if cl, ok := rng.X.(*ast.CompositeLit); ok {
			for _, e := range cl.Elts {
				if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					wired[strings.Trim(bl.Value, `"`)] = true
				}
			}
		}
		return true
	})

	return wired
}

// TestRevokeTypeForTable_CoversEveryWiredRevoker pins every table wired to a Revoker in main.go
// against revokeTypeForTable, and rejects stale map keys — the sibling of
// TestFinalTypeForTable_CoversEveryWiredSweeper.
func TestRevokeTypeForTable_CoversEveryWiredRevoker(t *testing.T) {
	wired := wiredRevokerTables(t)
	if len(wired) == 0 {
		t.Fatal("found no Revoker wiring in cmd/lens/main.go — the guard's AST walk has gone stale, not the wiring")
	}
	for table := range wired {
		rev, ok := revokeTypeForTable[table]
		if !ok {
			t.Errorf("main.go wires a Revoker over %q but revokeTypeForTable has no entry:\n"+
				"  its clawback rows would have no decided ledger type, so revokeOne fail-closes (OutcomeError).\n"+
				"  Add %q → its own SUPPLY-NEUTRAL *_revoked type, and keep that type OUT of GetTotalSupply's\n"+
				"  allow-list AND the burned list (a clawback of held is supply-neutral, not a burn).", table, table)
			continue
		}
		if rev == "" {
			t.Errorf("revokeTypeForTable[%q] is empty — a clawback row cannot carry an empty ledger type", table)
		}
	}
	// A stale key is a clawback label that no longer corresponds to a wired mint — it would silently
	// bless a typo'd table name. Every map key must be wired.
	for table := range revokeTypeForTable {
		if !wired[table] {
			t.Errorf("revokeTypeForTable has an entry for %q but cmd/lens/main.go wires no Revoker over it — "+
				"remove the stale key or wire the clawback", table)
		}
	}
}

// TestRevokeTypeForTable_MatchesFinalizeTables pins the design symmetry: settle and revoke are the
// two exits from held, so exactly the tables the finalize sweeper settles are the tables the Revoker
// claws back. Keeping the two type maps over identical key sets means #312's main.go pin
// (TestFinalTypeForTable_CoversEveryWiredSweeper) transitively backs this one too.
func TestRevokeTypeForTable_MatchesFinalizeTables(t *testing.T) {
	for table := range finalTypeForTable {
		if _, ok := revokeTypeForTable[table]; !ok {
			t.Errorf("finalTypeForTable settles %q but revokeTypeForTable cannot claw it back — "+
				"a settleable held mint must also be revocable (both are exits from held)", table)
		}
	}
	for table := range revokeTypeForTable {
		if _, ok := finalTypeForTable[table]; !ok {
			t.Errorf("revokeTypeForTable claws back %q but finalTypeForTable never settles it — "+
				"a revocable held mint must also be settleable (both are exits from held)", table)
		}
	}
}

// TestRevokeTypeForTable_NoRevokeTypeIsCounted is the MONEY-SAFETY guard, inverted from the finalize
// side: a clawback reverses held value that never entered circulation, so its type must NOT be in
// GetTotalSupply's allow-list — otherwise reversing a held mint would (absurdly) ADD to supply, or a
// stale reader would miscount. mining.CountedSupplyTypes is the allow-list GetTotalSupply itself
// reads, so this cannot drift from the real query.
func TestRevokeTypeForTable_NoRevokeTypeIsCounted(t *testing.T) {
	counted := map[string]bool{}
	for _, ty := range mining.CountedSupplyTypes() {
		counted[ty] = true
	}
	for table, rev := range revokeTypeForTable {
		if counted[rev] {
			t.Errorf("revokeTypeForTable[%q] = %q is COUNTED in GetTotalSupply — a held-mint clawback\n"+
				"  must be supply-neutral (the held LENS never entered circulation). Counting it would make a\n"+
				"  relabel move money — the inverse of the finalize contract, where a settled type MUST be counted.",
				table, rev)
		}
	}
}

// TestRevokeTypeForTable_NoRevokeTypeIsBurned pins the other half of the money contract: a clawback
// of held is NOT a burn of circulating supply, so its type must not be one of the two the burned
// aggregate (GetTotalBurned / GetCirculatingSupply) sums — TypeBurn or TypeStakeSlash. Adding a
// *_revoked type to the burned list would understate circulating supply by the clawed-back amount.
func TestRevokeTypeForTable_NoRevokeTypeIsBurned(t *testing.T) {
	for table, rev := range revokeTypeForTable {
		if rev == mining.TypeBurn || rev == mining.TypeStakeSlash {
			t.Errorf("revokeTypeForTable[%q] = %q is a BURNED type (TypeBurn/TypeStakeSlash) — a clawback of\n"+
				"  held value that never circulated must not register as a burn of circulating supply.", table, rev)
		}
	}
}

// TestRevokeTypeForTable_NoRevokeTypeIsAMintMoment mirrors the finalize guard: a revoke type that
// was ALSO a mint-moment type would re-run the verified-to-earn gate + rate cap when RevokeHeldTxAs
// flows through heldInner — gating a BURN, which is nonsensical. The mint moment is the _held type;
// the clawback must never be gated as one.
func TestRevokeTypeForTable_NoRevokeTypeIsAMintMoment(t *testing.T) {
	for table, rev := range revokeTypeForTable {
		if mining.IsMintType(rev) {
			t.Errorf("revokeTypeForTable[%q] = %q is in mintTypeList — a clawback would be re-gated + rate-capped\n"+
				"  as if it were a mint. The MINT moment is the _held type; the revoke must not be gated.", table, rev)
		}
	}
}
