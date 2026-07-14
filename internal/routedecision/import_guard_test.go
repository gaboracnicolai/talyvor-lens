package routedecision

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// MINT-FREE, mechanically enforced: the route-decision evidence store must reach no ledger/mint/economy path.
// It records descriptive cost EVIDENCE (an estimate), never value. If this test fails, the "it moves no money"
// claim in migration 0092 has been broken.
func TestImportGuard_RouteDecision_MintFree(t *testing.T) {
	forbiddenImports := []string{
		"internal/mining",      // the ledger / mint gate
		"internal/poolroyalty", // the minters
		"internal/povi",        // receipt minting
		"internal/economy",     // the reward loop
		"internal/provenance",  // stake bonds / slash
	}
	forbiddenIdents := map[string]bool{
		"CreditTx": true, "CreditHeldTx": true, "FinalizeHeldTx": true, "LedgerStore": true,
		"SlashStake": true, "SlashStakeTx": true, "LockStake": true, "MintFromReceipt": true,
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
					t.Errorf("%s imports %q — the evidence store must move no money", e.Name(), p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbiddenIdents[id.Name] {
				t.Errorf("%s references %q — routedecision mints/slashes nothing", e.Name(), id.Name)
			}
			return true
		})
	}
}
