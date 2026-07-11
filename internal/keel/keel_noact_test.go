package keel

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// KEEL NEVER ACTS — structural guard. The drift oracle is READ-ONLY over the corpus + append-only over its
// OWN findings; it must be import-incapable of touching money. This fails if any keel source file imports a
// ledger / held-balance / mint / economy / anchor package — the same discipline as
// poolroyalty.TestDetectorSweep_NeverActs_ImportGuard. (Lens has no local semgrep guard; this is the guard.)
func TestKeel_NeverActs_ImportGuard(t *testing.T) {
	forbidden := []string{
		"internal/economy",     // LENS/LXC ledger + balances
		"internal/poolroyalty", // held-ledger mint/finalize/revoke
		"internal/povi",        // stake/slash
		"internal/mining",      // credit/earn
		"internal/billing",     // money out
		"anchor", "ledger", "held", "mint", "credit", "stake", "slash",
	}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.Contains(path, bad) {
					t.Errorf("%s imports forbidden %q (keel must never touch money — read-only oracle)", name, path)
				}
			}
		}
	}
}
