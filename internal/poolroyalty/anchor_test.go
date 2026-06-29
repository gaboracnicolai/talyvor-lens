package poolroyalty

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CostAnchor reproduces today's #2 math EXACTLY: Value = Share × AvoidedCOGSUSD.
func TestCostAnchor_IsShareTimesAvoidedCOGS(t *testing.T) {
	for _, c := range []struct {
		share, cogs, want float64
	}{
		{0.5, 1.0, 0.5}, {0.5, 0, 0}, {1.0, 2.5, 2.5}, {0.0, 9.9, 0},
	} {
		a := CostAnchor{Share: c.share}
		if got := a.Value(GainInput{AvoidedCOGSUSD: c.cogs}); got != c.want {
			t.Errorf("CostAnchor{%v}.Value(cogs %v) = %v, want %v", c.share, c.cogs, got, c.want)
		}
	}
	if (CostAnchor{}).Kind() != "cost" {
		t.Error("CostAnchor.Kind must be \"cost\"")
	}
}

// HeldBenchmarkAnchor: Value = RatePerPoint × clamp01(HeldScore); bounded; requires a positive rate.
func TestHeldBenchmarkAnchor_Math_And_RequiresRate(t *testing.T) {
	a, ok := NewHeldBenchmarkAnchor(10.0)
	if !ok {
		t.Fatal("a positive rate must construct")
	}
	cases := []struct {
		score, want float64
	}{
		{1.0, 10.0}, {0.8, 8.0}, {0.0, 0}, {-0.3, 0}, {2.0, 10.0}, // clamp01: >1 → 1, ≤0 → 0
	}
	for _, c := range cases {
		if got := a.Value(GainInput{HeldScore: c.score}); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("HeldBenchmarkAnchor{10}.Value(score %v) = %v, want %v", c.score, got, c.want)
		}
	}
	if math.IsNaN(a.Value(GainInput{HeldScore: math.NaN()})) || a.Value(GainInput{HeldScore: math.NaN()}) != 0 {
		t.Error("NaN score must value to 0, never NaN")
	}
	if a.Kind() != "held_benchmark" {
		t.Error("Kind must be \"held_benchmark\"")
	}
	// REQUIRES a rate — no default that could silently mint.
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if _, ok := NewHeldBenchmarkAnchor(bad); ok {
			t.Errorf("NewHeldBenchmarkAnchor(%v) must REJECT (no default mint), got ok=true", bad)
		}
	}
}

// repoRoot walks up from the test dir until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("go.mod not found")
	return ""
}

// (proof 3) NO NEW MINT SURFACE — mechanically enforced: NewHeldBenchmarkAnchor and SetAnchor are
// CONSTRUCTED/CALLED in NO non-test .go file anywhere in the repo. The held-benchmark anchor is
// reachable only from tests this PR; the live minters keep the default CostAnchor.
func TestHeldBenchmarkAnchor_TestOnly_NoLiveSelection(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && (info.Name() == ".git" || info.Name() == "node_modules" || info.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// NewHeldBenchmarkAnchor(...) or x.SetAnchor(...) in a non-test file = a live selection.
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "NewHeldBenchmarkAnchor" {
					offenders = append(offenders, path+": NewHeldBenchmarkAnchor")
				}
			case *ast.SelectorExpr:
				if fn.Sel.Name == "NewHeldBenchmarkAnchor" || fn.Sel.Name == "SetAnchor" {
					offenders = append(offenders, path+": "+fn.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	if len(offenders) > 0 {
		t.Errorf("HeldBenchmarkAnchor/SetAnchor reached from a NON-test file (this PR wires no new mint surface):\n  %s", strings.Join(offenders, "\n  "))
	}
}

// (proof 4) NO-LOOP / money-boundary: anchor.go is pure valuation — it references no ledger/mint
// symbol and imports nothing that could write the score it prices.
func TestAnchor_NoLedgerNoMint_ImportGuard(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "anchor.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenImports := []string{"internal/mining", "internal/benchprobe", "jackc/pgx", "database/sql"}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		for _, bad := range forbiddenImports {
			if strings.Contains(p, bad) {
				t.Errorf("anchor.go imports %q — an anchor must be pure valuation (no ledger/DB/score-producer)", p)
			}
		}
	}
	forbidden := map[string]bool{"Credit": true, "CreditTx": true, "CreditHeldTx": true, "LedgerStore": true, "Begin": true, "Exec": true, "QueryRow": true}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && forbidden[id.Name] {
			t.Errorf("anchor.go references %q — anchors never touch the ledger/DB", id.Name)
		}
		return true
	})
}
