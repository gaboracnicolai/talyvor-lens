package attestation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// MINT-FREE: step (b) records a verified hardware class and mints NOTHING. This package must import no
// ledger/mint/anchor/economy symbol — it writes only node_attestations. (Step c is the mint.)
func TestImportGuard_Attestation_NoLedgerNoMint(t *testing.T) {
	forbiddenImports := []string{"internal/mining", "internal/poolroyalty", "internal/economy"}
	forbiddenIdents := map[string]bool{
		"CreditHeldTx": true, "CreditTx": true, "Credit": true, "LedgerStore": true,
		"NewHeldBenchmarkAnchor": true, "HeldBenchmarkAnchor": true,
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
					t.Errorf("%s imports %q — step (b) is verify+record, it must reach no ledger/mint", e.Name(), p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbiddenIdents[id.Name] {
				t.Errorf("%s references %q — step (b) mints nothing", e.Name(), id.Name)
			}
			return true
		})
	}
}
