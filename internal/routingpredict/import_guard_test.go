package routingpredict

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// (proof 5) NO MINT / NO SERVE-PATH COUPLING — mechanically enforced: the prediction substrate must
// reference no mint/ledger/anchor symbol and must NOT import internal/routing (the live serve/Advisor
// path) nor any economy package. PR-1 is inert data; the scoring (PR-3) and mint (PR-4) live elsewhere.
func TestImportGuard_RoutingPredict_NoMintNoServePath(t *testing.T) {
	forbiddenImports := []string{
		"internal/routing",     // the live serve/Advisor path — must stay decoupled
		"internal/mining",      // the ledger / mint gate
		"internal/poolroyalty", // the minters + HeldBenchmarkAnchor
		"internal/povi",        // receipt minting
		"internal/economy",     // the reward loop
	}
	forbiddenIdents := map[string]bool{
		"CreditTx": true, "CreditHeldTx": true, "MintFromReceipt": true, "LedgerStore": true,
		"NewHeldBenchmarkAnchor": true, "HeldBenchmarkAnchor": true, "Anchor": true,
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
					t.Errorf("%s imports %q — the prediction substrate must reach no serve/mint path", e.Name(), p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbiddenIdents[id.Name] {
				t.Errorf("%s references %q — PR-1 mints/scores nothing", e.Name(), id.Name)
			}
			return true
		})
	}
}
