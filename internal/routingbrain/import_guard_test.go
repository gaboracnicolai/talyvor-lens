package routingbrain

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestRoutingBrain_MintFree_ImportGuard — the architectural mint-free guarantee: the
// brain imports NO money/ledger/mint package, so no code path can mint, debit, or
// write a ledger row. Checks ACTUAL imports via go/parser (doctrine comments don't
// false-trip). The internal packages it MAY import — worktier (H1), modelcapability
// (H2), keel — are themselves DESCRIPTIVE and import-guarded mint-free.
func TestRoutingBrain_MintFree_ImportGuard(t *testing.T) {
	forbidden := []string{
		"talyvor/lens/internal/mining", "talyvor/lens/internal/economy",
		"talyvor/lens/internal/poolroyalty", "talyvor/lens/internal/povi",
		"talyvor/lens/internal/billing", "talyvor/lens/internal/ledger",
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
					t.Errorf("%s imports %q — routingbrain is DESCRIPTIVE and must be mint-free", e.Name(), path)
				}
			}
		}
	}
}
