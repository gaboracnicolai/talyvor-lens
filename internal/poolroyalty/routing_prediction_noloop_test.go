package poolroyalty

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// (proof 7) NO-LOOP — the routing-prediction minter must never feed back into the score it is paid on.
// It READS routing_prediction_scores (the scorer's output) by SQL and WRITES only the ledger +
// routing_prediction_mints. It must NOT import the scorer (internal/routingscore), the routing-weight /
// Advisor layer (internal/routing), the serve path (internal/proxy), or the real-inference layer
// (internal/inference) — importing any of those would let a mint reach back into scoring/weights and
// create a mint→score→mint loop. internal/mining (the ledger) is REQUIRED (CreditHeldTx) and ALLOWED.
//
// File-scoped (not package-scoped): the poolroyalty package legitimately imports internal/mining, so the
// guard parses ONLY routing_prediction_minter.go and checks ITS imports + that it writes no scorer/weights
// table (it INSERTs only routing_prediction_mints; routing_prediction_scores is read-only here).
func TestImportGuard_RoutingPredictionMinter_NoLoop(t *testing.T) {
	const minterFile = "routing_prediction_minter.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, minterFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	forbiddenImports := []string{
		"internal/routingscore", // the scorer — reading it (vs the table) would couple mint to scoring
		"internal/routing",      // routing weights / Advisor (the baseline) — never reached from the mint
		"internal/proxy",        // the serve path
		"internal/inference",    // the real-inference layer
	}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		for _, bad := range forbiddenImports {
			if strings.Contains(p, bad) {
				t.Errorf("%s imports %q — the routing-prediction minter must reach the scorer/weights only by reading its TABLE, never by import (NO-LOOP)", minterFile, p)
			}
		}
		if strings.Contains(p, "internal/mining") {
			t.Errorf("%s imports internal/mining directly — it must reach the ledger ONLY through the injected ledgerCreditTx interface (as the eval minter does)", minterFile)
		}
	}

	// NO WRITE to the scorer's tables: the minter may READ routing_prediction_scores, but any INSERT/UPDATE
	// into routing_prediction_scores / routing_predictions / routing_patterns would be a feedback write.
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		sql := strings.ToLower(lit.Value)
		for _, writeTbl := range []string{"routing_prediction_scores", "routing_predictions", "routing_patterns"} {
			if strings.Contains(sql, "insert into "+writeTbl) || strings.Contains(sql, "update "+writeTbl) || strings.Contains(sql, "delete from "+writeTbl) {
				t.Errorf("%s writes %q — the minter must only READ scores and WRITE routing_prediction_mints + the ledger (NO-LOOP)", minterFile, writeTbl)
			}
		}
		return true
	})
}
