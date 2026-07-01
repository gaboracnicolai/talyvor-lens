package poolroyalty

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// (proof 9) NO-LOOP / money-boundary: the latency minter READS node_cohort_latency_stats ⋈ inference_nodes ⋈
// benchmark_node_scores ⋈ workspace_card_fingerprints by RAW SQL and WRITES only node_latency_mints + the
// ledger. It must import NONE of the score/capture producers — so a mint can never feed the latency_ewma or
// benchmark score it is paid on. It must reference no score-producer symbol either.
func TestLatencyMinter_NoLoop_ImportGuard(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "latency_minter.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The minter must not import the capture/score/serve producers (would enable a mint→signal loop) or a
	// second DB/ledger surface beyond pgx.
	forbiddenImports := []string{
		"internal/nodelatency",  // the latency capture (writes node_cohort_latency_stats)
		"internal/benchprobe",   // the benchmark-score producer (writes benchmark_node_scores)
		"internal/routingscore", // sibling score producer
		"internal/proxy",        // the serve path
		"internal/inference",    // the upstream caller
	}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		for _, bad := range forbiddenImports {
			if strings.Contains(p, bad) {
				t.Errorf("latency_minter.go imports %q — the minter must not reach the producer of the signal it pays on (NO-LOOP)", p)
			}
		}
	}
	// It must not name the capture/score types/functions.
	forbidden := map[string]bool{
		"RecordServe": true, "NodeScore": true, "UpsertNodeScore": true, "DeriveInputCohort": true,
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && forbidden[sel.Sel.Name] {
			t.Errorf("latency_minter.go references %q — it must not call a score/capture producer (NO-LOOP)", sel.Sel.Name)
		}
		return true
	})
}
