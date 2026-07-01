package nodelatency

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// (proof 5) MINT-FREE / NO-LEDGER: the node-latency capture is DESCRIPTIVE — it writes only
// node_cohort_latency_stats and must reach no ledger/mint/anchor. It must NOT import internal/mining
// (ledger + mint gate), internal/poolroyalty (minters + HeldBenchmarkAnchor), internal/economy, or
// internal/povi, and must reference no credit/anchor/ledger ident. The Store's Exec-only surface is the
// architectural half; this AST guard is the other half.
func TestImportGuard_NodeLatency_NoLedgerNoMint(t *testing.T) {
	forbiddenImports := []string{
		"internal/mining",
		"internal/poolroyalty",
		"internal/economy",
		"internal/povi",
	}
	forbiddenIdents := map[string]bool{
		"Credit": true, "CreditTx": true, "CreditHeldTx": true, "MintFromReceipt": true, "LedgerStore": true,
		"NewHeldBenchmarkAnchor": true, "HeldBenchmarkAnchor": true, "SetAnchor": true,
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
					t.Errorf("%s imports %q — node-latency capture is descriptive; it must reach no ledger/mint/anchor", e.Name(), p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbiddenIdents[id.Name] {
				t.Errorf("%s references %q — the capture writes only node_cohort_latency_stats; it mints nothing", e.Name(), id.Name)
			}
			return true
		})
	}
}
