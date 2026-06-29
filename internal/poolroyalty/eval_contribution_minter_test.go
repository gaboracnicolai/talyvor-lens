package poolroyalty

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// (proof 4, unit half) INERT BY DEFAULT: rate 0 ⇒ NewHeldBenchmarkAnchor refuses ⇒ anchor nil ⇒
// RunOnce is a TOTAL no-op even with the enable flag ON and a nil DB (proving it touches no DB).
func TestEvalContributionMinter_Rate0_IsInert(t *testing.T) {
	m := NewEvalContributionMinter(nil, nil, 0, func() bool { return true }) // rate 0, "both flags on"
	if m.anchor != nil {
		t.Fatal("rate 0 must leave the anchor nil (inert) — NewHeldBenchmarkAnchor refuses a non-positive rate")
	}
	n, err := m.RunOnce(context.Background()) // nil db: if it tried to query, it would panic — proves no DB access
	if err != nil || n != 0 {
		t.Fatalf("rate-0 minter must no-op (no DB access, no mint): n=%d err=%v", n, err)
	}
	// A positive rate DOES construct the anchor (so the only thing standing between inert and live is the rate).
	live := NewEvalContributionMinter(nil, nil, 10, func() bool { return true })
	if live.anchor == nil {
		t.Fatal("a positive rate must construct the held-benchmark anchor")
	}
}

// (proof 8) NO-LOOP / money-boundary: the eval-contribution minter must READ benchmark_probes but
// NEVER write benchmark_probes or benchmark_node_scores — so a mint can never feed the score it is
// paid on. Asserted structurally over the source: no INSERT/UPDATE/DELETE against either score table.
func TestEvalContributionMinter_NoScoreWrites_Guard(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "eval_contribution_minter.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	// It must not import benchprobe (the score producer) — the NO-LOOP package boundary.
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(p, "internal/benchprobe") {
			t.Errorf("eval_contribution_minter.go must NOT import benchprobe (NO-LOOP: read the score by SQL, never via the producer)")
		}
	}
	// Scan every string literal for a write against the score tables.
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		sql := strings.ToLower(lit.Value)
		for _, tbl := range []string{"benchmark_probes", "benchmark_node_scores"} {
			for _, verb := range []string{"insert into " + tbl, "update " + tbl, "delete from " + tbl} {
				if strings.Contains(sql, verb) {
					t.Errorf("eval_contribution_minter.go WRITES the score table (%q) — NO-LOOP violation: the mint must only CONSUME the score", verb)
				}
			}
		}
		return true
	})
}
