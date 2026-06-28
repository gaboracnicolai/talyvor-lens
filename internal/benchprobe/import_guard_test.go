package benchprobe

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// (6) NO-LOOP / money-boundary: the proof-of-benchmark measurement package must reference NO
// ledger/mint/earn symbol — the score is computed ONLY from eval.StaticScore(output, expected), never
// from mint volume, and the package never imports a money path. This AST guard fails if a future edit
// imports mining/poolroyalty/economy or names a Credit/ledger/mint symbol.
func TestImportGuard_NoLedgerNoMint(t *testing.T) {
	forbiddenImports := []string{
		"internal/mining",
		"internal/poolroyalty",
		"internal/economy",
		"internal/earnverify",
	}
	forbiddenIdents := map[string]bool{
		"Credit": true, "CreditTx": true, "CreditHeldTx": true, "LedgerStore": true,
		"MintFromReceipt": true, "applyTx": true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbiddenImports {
				if strings.Contains(p, bad) {
					t.Errorf("%s imports %q — measurement must not reach a ledger/mint path", name, p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbiddenIdents[id.Name] {
				t.Errorf("%s references money symbol %q — measurement must be ledger/mint-free", name, id.Name)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no source files scanned")
	}
}
