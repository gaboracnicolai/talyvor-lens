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

// (proof 7) EXACTLY TWO LIVE CALLERS — mechanically enforced: NewHeldBenchmarkAnchor is constructed in
// EXACTLY TWO non-test .go files — the two sanctioned Proof-of-Improvement mints (instance 1, proof-of-
// eval-contribution; instance 2, proof-of-routing-prediction) and NO other — and there is NO stray live
// SetAnchor call anywhere. PR #248 shipped this anchor reachable from nothing; PR #250 added the first
// live home and PR-4 the second. A THIRD caller — any other mint trying to select the held-benchmark
// anchor without its own review — turns this red.
func TestHeldBenchmarkAnchor_ExactlyTwoLiveCallers(t *testing.T) {
	root := repoRoot(t)
	// the sanctioned live callers (by filename); each must appear exactly once, and no other file may call.
	sanctioned := map[string]bool{
		"eval_contribution_minter.go":  true, // P-o-I instance 1
		"routing_prediction_minter.go": true, // P-o-I instance 2
	}
	var newCallers, setAnchorCallers []string
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
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "NewHeldBenchmarkAnchor" {
					newCallers = append(newCallers, path)
				}
			case *ast.SelectorExpr:
				if fn.Sel.Name == "NewHeldBenchmarkAnchor" {
					newCallers = append(newCallers, path)
				}
				if fn.Sel.Name == "SetAnchor" {
					setAnchorCallers = append(setAnchorCallers, path) // no live SetAnchor: the eval minter takes the rate in its ctor
				}
			}
			return true
		})
		return nil
	})
	if len(newCallers) != 2 {
		t.Errorf("NewHeldBenchmarkAnchor must have EXACTLY TWO non-test callers, got %d: %v", len(newCallers), newCallers)
	}
	seen := map[string]int{}
	for _, c := range newCallers {
		matched := false
		for name := range sanctioned {
			if strings.HasSuffix(c, name) {
				seen[name]++
				matched = true
			}
		}
		if !matched {
			t.Errorf("unsanctioned NewHeldBenchmarkAnchor caller %s — only the two P-o-I minters may construct the held-benchmark anchor", c)
		}
	}
	for name := range sanctioned {
		if seen[name] != 1 {
			t.Errorf("sanctioned caller %s must appear EXACTLY once, got %d", name, seen[name])
		}
	}
	if len(setAnchorCallers) != 0 {
		t.Errorf("no live SetAnchor call is sanctioned (each minter constructs its own anchor), got: %v", setAnchorCallers)
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
