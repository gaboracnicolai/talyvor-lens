package routingscore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// (proof 5) NO SERVE-PATH / NO REAL INFERENCE SURFACE — the reshape guarantee: routingscore must NOT
// import internal/proxy (the serve path) — it depends ONLY on the Inferer interface, which has no live
// implementation in PR-3a. (proof 6) NO-LOOP / NO MINT: it references no output_quality (the candidate-C
// loop metric) and no mint/anchor/ledger symbol; it reads only held eval.StaticScore.
func TestImportGuard_RoutingScore_NoServePathNoMintNoOutputQuality(t *testing.T) {
	forbiddenImports := []string{
		"internal/proxy",       // the serve path — the whole point of the PR-3a reshape (no serve touch)
		"internal/inference",   // the Option-Y extraction is PR-3b, not here
		"internal/mining",      // ledger / mint gate
		"internal/poolroyalty", // minters + HeldBenchmarkAnchor
		"internal/povi",
		"internal/economy",
	}
	forbiddenIdents := map[string]bool{
		"output_quality": true, "OutputQuality": true, // the candidate-C loop metric — never read
		"CreditTx": true, "CreditHeldTx": true, "MintFromReceipt": true, "LedgerStore": true,
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
					t.Errorf("%s imports %q — routingscore must not reach the serve path / mint / real-inference layer", e.Name(), p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbiddenIdents[id.Name] {
				t.Errorf("%s references %q — score reads held StaticScore only; mints/loops nothing", e.Name(), id.Name)
			}
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(strings.ToLower(lit.Value), "output_quality") {
				t.Errorf("%s contains an output_quality SQL reference — NO-LOOP: the score must read held StaticScore only", e.Name())
			}
			return true
		})
	}
}
