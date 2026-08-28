package shadowmint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE GUARD THIS PACKAGE'S OWN COMMENT SAID ALREADY EXISTED.
//
// recorder.go states, of insertSQL: "It names lens_shadow_mints and nothing else; a test in
// internal/mining pins that the shadow path never names the ledger or balance tables."
//
// ⚠ THAT TEST DOES NOT DO THAT. `internal/mining.TestShadowSink_CannotReachTheLedger` parses
// mining's own shadow.go and asserts the ShadowSink INTERFACE takes no pgx.Tx / LedgerStore /
// DualTokenStore / pgx.Conn parameter. It is a statement about a method signature. It never reads
// this package, and no implementation has to obey it to reach the ledger — because the concrete
// sink is handed something strictly more powerful than a pgx.Tx at construction:
//
//	tokenLedger.SetShadowSink(shadowmint.New(pool), mining.ShadowableMintTypes())   // cmd/lens/main.go
//
// a *pgxpool.Pool for the same database. The interface guard bans the narrow door; the wide one
// was already open. This package's header — "It cannot credit anyone because it holds nothing that
// could" — was the one line describing the thing that actually decides it, and it was wrong: it
// holds a pool, which is everything that could.
//
// ⚠⚠ MEASURED, NOT ARGUED. A schema-valid `INSERT INTO lens_token_ledger (workspace_id, amount,
// balance_after, type, description)` was added to RecordShadow — crediting a workspace for the
// exact mint the system had just refused — and the FULL suite was run as CI runs it
// (`go test -timeout 120s -race -count=1 -p 1 ./...`, real Postgres from zero, 124 migrations):
//
//	106 packages ok, 0 FAIL, go vet clean.
//
// TestShadowSink_CannotReachTheLedger passed throughout. An earlier attempt named a column the
// ledger does not have and WAS caught — by internal/schemasql's parity guard, i.e. by a typo
// rather than by anything that knows what a shadow mint is. That near-miss is why this guard reads
// table names and not statement shapes.
//
// ⚠⚠⚠ IT MUST READ STRING LITERALS, NOT TEXT. recorder.go legitimately names lens_token_ledger in
// TWO COMMENTS (the migration pointer and the no-pgx.Tx note), so a grep-based rule would fail on
// correct code and be deleted within a week. Prose may discuss the ledger; SQL may not name it.

// bannedTables are the relations a shadow recorder must never write. A shadow row is an
// observation; these are where financial facts live.
var bannedTables = []string{
	"lens_token_ledger",
	"lens_token_balances",
	"traffic_mint_holds",
}

// tableRef finds the relation named by each SQL clause that can reach a table.
var tableRef = regexp.MustCompile(`(?is)\b(?:insert\s+into|update|delete\s+from|from|join)\s+([a-z_][a-z0-9_.]*)`)

// sqlVerb is how a literal is recognised as SQL at all, so the floor below can count statements
// rather than strings.
var sqlVerb = regexp.MustCompile(`(?is)\b(?:insert\s+into|update\s+|delete\s+from|select\s+)`)

func packageStringLiterals(t *testing.T) (lits []string, files int) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files++
		// ⚠ ast.Inspect over BasicLit reaches string literals ONLY. Comments are attached to the
		// file's Comments list and are never visited here, which is exactly the distinction this
		// guard needs: recorder.go discusses the ledger in prose and must stay legal.
		ast.Inspect(f, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(bl.Value)
			if err != nil {
				s = bl.Value
			}
			lits = append(lits, s)
			return true
		})
	}
	return lits, files
}

func TestShadowRecorderNamesOnlyItsOwnTable(t *testing.T) {
	lits, files := packageStringLiterals(t)

	var statements []string
	tables := map[string]bool{}
	for _, s := range lits {
		if !sqlVerb.MatchString(s) {
			continue
		}
		statements = append(statements, s)
		for _, m := range tableRef.FindAllStringSubmatch(s, -1) {
			tables[strings.ToLower(m[1])] = true
		}
	}

	// ⚠ NON-VACUITY FLOORS. A parser that returned no files, a package whose SQL moved into a
	// constant this scan cannot see, or a regexp that stopped matching, would each make every
	// assertion below pass by having nothing to judge — and would look exactly like a compliant
	// package. Deliberately floors, not exact pins: this package may legitimately grow a file.
	if files < 1 {
		t.Fatalf("parsed %d non-test .go files in this package — the scan found nothing to check", files)
	}
	if len(statements) < 1 {
		t.Fatalf("found %d SQL statements among %d string literals in %d file(s); this package runs at "+
			"least one and a scan that sees none is not measuring the package", len(statements), len(lits), files)
	}
	if !tables["lens_shadow_mints"] {
		t.Fatalf("the scan did not find lens_shadow_mints among %v — it is not reading this package's "+
			"real SQL, so its silence about the ledger means nothing", keys(tables))
	}

	for _, banned := range bannedTables {
		if tables[banned] {
			t.Errorf("shadowmint SQL names %q. A shadow row records what a mint WOULD have paid; "+
				"writing it to a financial table credits the mint the system just refused. "+
				"The Recorder holds a *pgxpool.Pool, so nothing but this test stops it.", banned)
		}
	}

	// The comment's actual claim: "It names lens_shadow_mints and nothing else."
	for tbl := range tables {
		if tbl != "lens_shadow_mints" {
			t.Errorf("shadowmint SQL names %q; this package is documented as naming lens_shadow_mints "+
				"and nothing else. Add it to bannedTables or correct the package comment — but the "+
				"comment and the SQL must not disagree.", tbl)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
