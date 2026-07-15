package modelcapability

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestModelCapability_MintFree_ImportGuard — the architectural mint-free
// guarantee: this package imports NO minting package, so no capability-curve code
// path can reach a ledger credit. (The store's db field is Exec/Query-only — no
// Begin — the complementary compile-time half.) Checks ACTUAL imports via
// go/parser, so doctrine comments don't false-trip. A future edit that imports any
// minter fails this. The one internal import it MAY have is worktier (H1) — itself
// mint-free and import-guarded.
func TestModelCapability_MintFree_ImportGuard(t *testing.T) {
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
					t.Errorf("%s imports %q — modelcapability is DESCRIPTIVE and must be mint-free (no minter imports)", e.Name(), path)
				}
			}
		}
	}
}
