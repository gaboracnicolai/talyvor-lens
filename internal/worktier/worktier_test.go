package worktier

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestClassify_Boundaries pins the enum edges of every axis (the frozen
// contract). Thresholds are implementation, but a consumer switches on these
// values — so the boundary mapping must be deterministic.
func TestClassify_Boundaries(t *testing.T) {
	// Size — total = input+output (here all input).
	for _, c := range []struct {
		total int
		want  SizeBucket
	}{{0, SizeSmall}, {999, SizeSmall}, {1000, SizeMedium}, {9999, SizeMedium},
		{10000, SizeLarge}, {99999, SizeLarge}, {100000, SizeXLarge}, {500000, SizeXLarge}} {
		if got := Classify(c.total, 0, 0, 0, false, false, "full").Size; got != c.want {
			t.Errorf("size(%d) = %q, want %q", c.total, got, c.want)
		}
	}
	// Cost — USD edges.
	for _, c := range []struct {
		usd  float64
		want CostBucket
	}{{0, CostTrivial}, {0.0009, CostTrivial}, {0.001, CostLow}, {0.0099, CostLow},
		{0.01, CostModerate}, {0.099, CostModerate}, {0.10, CostHigh}, {5, CostHigh}} {
		if got := Classify(0, 0, c.usd, 0, false, false, "full").Cost; got != c.want {
			t.Errorf("cost($%.4f) = %q, want %q", c.usd, got, c.want)
		}
	}
	// Complexity — score [0,5] edges.
	for _, c := range []struct {
		score int
		want  Complexity
	}{{0, ComplexityTrivial}, {1, ComplexitySimple}, {2, ComplexitySimple},
		{3, ComplexityModerate}, {4, ComplexityModerate}, {5, ComplexityComplex}} {
		if got := Classify(0, 0, 0, c.score, false, false, "full").Complexity; got != c.want {
			t.Errorf("complexity(%d) = %q, want %q", c.score, got, c.want)
		}
	}
	// Sensitivity — precedence restricted > elevated > normal, incl. the OR.
	for _, c := range []struct {
		pii, guard bool
		policy     string
		want       Sensitivity
	}{
		{false, false, "full", SensitivityNormal},
		{false, false, "metadata", SensitivityNormal},
		{true, false, "full", SensitivityElevated},    // PII
		{false, true, "full", SensitivityElevated},    // guardrail (the OR)
		{true, true, "full", SensitivityElevated},     // both
		{false, false, "none", SensitivityRestricted}, // policy wins with no PII/guardrail
		{true, true, "none", SensitivityRestricted},   // policy PRECEDENCE over elevated
	} {
		if got := Classify(0, 0, 0, 0, c.pii, c.guard, c.policy).Sensitivity; got != c.want {
			t.Errorf("sensitivity(pii=%v guard=%v policy=%q) = %q, want %q", c.pii, c.guard, c.policy, got, c.want)
		}
	}
}

// TestClassify_SizeUsesTotalNotSplit — input+output sum drives size; two shapes
// with the same total bucket identically (the split is kept in the RAW columns,
// not the bucket).
func TestClassify_SizeUsesTotal(t *testing.T) {
	a := Classify(50000, 200, 0, 0, false, false, "full").Size // 50,200
	b := Classify(200, 50000, 0, 0, false, false, "full").Size // 50,200
	if a != SizeLarge || b != SizeLarge {
		t.Errorf("both 50,200-total shapes must be large; got %q / %q", a, b)
	}
}

// TestWorkTier_MintFree_ImportGuard — the architectural mint-free guarantee: the
// worktier package imports NO minting package, so no code path here can reach a
// ledger credit. (The store's db field is Exec/Query-only — no Begin — which is
// the complementary compile-time half.) A future edit that imports a minter
// trips this.
func TestWorkTier_MintFree_ImportGuard(t *testing.T) {
	// Checks ACTUAL imports (via go/parser), not raw text — so the mint-free
	// doctrine comments don't false-trip. A future edit that imports any minter
	// package fails this.
	forbidden := []string{
		"talyvor/lens/internal/mining", "talyvor/lens/internal/economy",
		"talyvor/lens/internal/poolroyalty", "talyvor/lens/internal/povi",
		"talyvor/lens/internal/billing",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.Contains(path, bad) {
					t.Errorf("%s imports %q — worktier is DESCRIPTIVE and must be mint-free (no minter imports)", e.Name(), path)
				}
			}
		}
	}
}
