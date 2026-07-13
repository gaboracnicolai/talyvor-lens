package outputverify

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// OUTPUTVERIFY NEVER ACTS — structural guard. The verifier is READ-ONLY over the request/response bytes it
// is handed + append-only over its OWN verdict table; it must be import-incapable of touching money. It may
// import the PURE eval scorer (internal/eval — no ledger) but NOTHING that mints/slashes/credits. This
// fails if any source file imports a ledger/economy/mint/held/stake/slash package — same discipline as
// keel.TestKeel_NeverActs_ImportGuard. This run produces a VERDICT, never money.
func TestOutputVerify_NeverActs_ImportGuard(t *testing.T) {
	forbidden := []string{
		"internal/economy",
		"internal/mining",
		"internal/poolroyalty",
		"internal/povi",
		"internal/billing",
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
					t.Errorf("%s imports forbidden %q (outputverify must never touch money — it emits a verdict, nothing else)", name, path)
				}
			}
		}
	}
}
