package haircutobs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// READ-ONLY observability, mechanically enforced: the haircut viewer must reach no ledger/mint constructor and
// no burn/slash primitive. It reads ledger metadata for display; it moves no money.
func TestImportGuard_HaircutObs_ReadOnly(t *testing.T) {
	forbiddenImports := []string{
		"internal/mining", "internal/poolroyalty", "internal/povi", "internal/economy", "internal/provenance",
	}
	forbiddenIdents := map[string]bool{
		"CreditTx": true, "CreditHeldTx": true, "SlashStake": true, "SlashStakeTx": true,
		"RevokeHeldTx": true, "LockStake": true, "LedgerStore": true,
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
					t.Errorf("%s imports %q — the haircut viewer is read-only, no mint reach", e.Name(), p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbiddenIdents[id.Name] {
				t.Errorf("%s references %q — haircutobs displays, it never moves money", e.Name(), id.Name)
			}
			return true
		})
	}
}
