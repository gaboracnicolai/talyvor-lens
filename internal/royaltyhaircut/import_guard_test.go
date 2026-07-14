package royaltyhaircut

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// REDUCE-ONLY, NEVER BURN/SLASH — mechanically enforced. The drift oracle returns a multiplier; it must reach
// NO burn/slash primitive and import no ledger/stake package. Even a compromised oracle can only ever produce
// a smaller (floored) reduction — never destroy stake. This is the H5 lesson in code: a haircut is not a slash.
func TestImportGuard_RoyaltyHaircut_NoBurnNoSlash(t *testing.T) {
	forbiddenImports := []string{
		"internal/mining",     // avoid a cycle AND prove no ledger reach
		"internal/povi",       // stake slashing
		"internal/provenance", // bond slash
		"internal/poolroyalty",
	}
	forbiddenIdents := map[string]bool{
		"SlashStake": true, "SlashStakeTx": true, "RevokeHeldTx": true, "RevokeHeldTxAs": true,
		"LockStake": true, "CreditHeldTx": true, "Burn": true,
	}
	fset := token.NewFileSet()
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbiddenImports {
				if strings.Contains(p, bad) {
					t.Errorf("%s imports %q — the haircut oracle must reach no burn/slash path", e.Name(), p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbiddenIdents[id.Name] {
				t.Errorf("%s references %q — a haircut REDUCES a mint, it never burns or slashes", e.Name(), id.Name)
			}
			return true
		})
	}
}
