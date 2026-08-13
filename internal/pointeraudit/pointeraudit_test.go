package pointeraudit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ── the scanner ──────────────────────────────────────────────────────────────
//
// Both rules read the SAME walk, deliberately: a rule that reads its own private
// scan of the tree can be satisfied by a scan that found nothing, and rule C
// below is the floor that says this one did not.

type citation struct {
	from     string // repo-relative path of the citing file
	line     int    // line in the citing file (for the failure message only — never pinned)
	raw      string // the citation as written
	target   string // the cited path as written
	lineNo   int    // 0 for a symbol citation
	symbol   string // "" for a line citation
	resolved string // repo-relative path of the cited file, or "" if it names nothing here
}

// repoRoot walks up from this package until it finds go.mod. Tests run in their
// own package directory, so the root is not the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not find go.mod above the test's working directory")
	return ""
}

func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(out)
	return out
}

// resolve maps a cited path to a file in THIS tree, or "" when it names none.
//
// ⚠ THE AMBIGUITY IS HANDLED RATHER THAN IGNORED, because getting it wrong moves
// the census in both directions. A citation written as a bare basename resolves
// only when EXACTLY ONE file in the tree carries that name: talyvor-track's
// lensintegration client is cited from here and this tree has its own client files,
// so a basename match would claim a pointer this guard cannot
// check. Those cross-repo citations are the half of the class that is unverifiable
// from inside one repo — #153's finding — and they are deliberately NOT counted.
func resolve(cited string, files []string) string {
	cited = strings.TrimPrefix(cited, "./")
	var hits []string
	for _, f := range files {
		if f == cited || strings.HasSuffix(f, "/"+cited) {
			hits = append(hits, f)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}

// scan finds every path-colon-line and path-hash-symbol citation written inside a
// comment. Spelled out rather than shown, for the reason doc.go records: a literal
// example here is a citation, and the scanner cannot tell it from a real one.
func scan(t *testing.T, root string, files []string) []citation {
	t.Helper()
	var out []citation
	fset := token.NewFileSet()
	for _, rel := range files {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			// A file this package cannot parse is not silently skipped: an
			// unparseable file is an unscanned file, and rule C's floor is a
			// count, so a silent skip shrinks the population invisibly.
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				lineNo := fset.Position(c.Pos()).Line
				for _, cit := range parseComment(c.Text) {
					cit.from, cit.line = rel, lineNo
					cit.resolved = resolve(cit.target, files)
					out = append(out, cit)
				}
			}
		}
	}
	return out
}

// parseComment pulls citations out of one comment's text. Hand-rolled rather
// than regexp so the two forms cannot silently share a match.
func parseComment(text string) []citation {
	var out []citation
	for i := 0; i+3 <= len(text); i++ {
		if !strings.HasPrefix(text[i:], ".go") {
			continue
		}
		// walk left over the path
		start := i
		for start > 0 && isPathByte(text[start-1]) {
			start--
		}
		if start == i {
			continue // ".go" with no name in front of it
		}
		path := text[start : i+3]
		rest := text[i+3:]
		switch {
		case strings.HasPrefix(rest, ":") && len(rest) > 1 && rest[1] >= '0' && rest[1] <= '9':
			j := 1
			for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
				j++
			}
			n, _ := strconv.Atoi(rest[1:j])
			out = append(out, citation{raw: path + rest[:j], target: path, lineNo: n})
		case strings.HasPrefix(rest, "#") && len(rest) > 1 && isSymbolByte(rest[1]):
			j := 1
			for j < len(rest) && isSymbolByte(rest[j]) {
				j++
			}
			out = append(out, citation{raw: path + rest[:j], target: path, symbol: rest[1:j]})
		}
	}
	return out
}

func isPathByte(b byte) bool {
	return b == '/' || b == '_' || b == '-' || b == '.' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isSymbolByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// declaredSymbols returns every top-level func, method, type, var and const name
// a file declares. A method is keyed by its own name: a citation naming forward in
// proxy means `func (p *Proxy) forward`, which is what a reader means by it.
func declaredSymbols(t *testing.T, root, rel string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, nil, 0)
	if err != nil {
		src, rerr := os.ReadFile(filepath.Join(root, rel))
		if rerr != nil {
			t.Fatalf("read %s: %v", rel, rerr)
		}
		f, err = parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
	}
	out := map[string]bool{}
	for _, d := range f.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			out[decl.Name.Name] = true
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					out[s.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range s.Names {
						out[n.Name] = true
					}
				}
			}
		}
	}
	return out
}

// ── RULE A — a symbol citation must resolve ─────────────────────────────────
//
// This is the form the line numbers are being converted TO, so it is the rule
// that has to hold for the conversion to be worth anything. It is exact: the
// file is named, the declaration is named, and both are checkable here.
func TestPointerAudit_EverySymbolCitationResolves(t *testing.T) {
	root := repoRoot(t)
	files := goFiles(t, root)
	cits := scan(t, root, files)

	symbolCits := 0
	for _, c := range cits {
		if c.symbol == "" {
			continue
		}
		symbolCits++
		if c.resolved == "" {
			t.Errorf("%s:%d cites %q and no single file in this tree is called %s",
				c.from, c.line, c.raw, c.target)
			continue
		}
		if !declaredSymbols(t, root, c.resolved)[c.symbol] {
			t.Errorf("%s:%d cites %q but %s declares no %s — a symbol citation that does not "+
				"resolve is the same false pointer a line number becomes, only quieter",
				c.from, c.line, c.raw, c.resolved, c.symbol)
		}
	}
	// THE FLOOR FOR THIS RULE. Every citation could be deleted and the loop above
	// would pass over an empty set. It is small on purpose: it is a floor, not a
	// target — the number that matters is rule B's, which only goes down.
	if symbolCits < 3 {
		t.Errorf("only %d symbol citations found; this rule is reading almost nothing. "+
			"If they were genuinely removed, lower this floor deliberately", symbolCits)
	}
}

// ── RULE B — the pinned census of what has NOT been converted ───────────────
//
// PER CITING FILE, NEVER PER LINE. Pinning `file:line -> target` would decay
// exactly the way the citations do: any edit above a comment moves it, and the
// guard would red for the one reason it exists to say is not a defect. The count
// is stable under edits inside a file and moves only when a citation is added or
// converted.
//
// ⚠ WHAT THE REMAINDER IS, SO THE NUMBER IS NOT READ AS NEGLIGENCE. Most of the
// survivors point at a STATEMENT inside proxy.go's serve() — the budget gate, the
// LXC gate, the reservation hold, the post-serve estimate. A symbol cannot name a
// statement inside a 900-line function, so converting them means either splitting
// serve or inventing a second citation grammar, and both are decisions rather than
// a session's tidy-up. They are pinned rather than blessed.
var lineCitationCensus = map[string]int{
	"internal/api/track_read_credential_test.go":                   1,
	"internal/benchprobe/store.go":                                 1,
	"internal/cohort/cohort_test.go":                               1,
	"internal/keel/keel_hardened.go":                               1,
	"internal/poolroyalty/distill_minter.go":                       1,
	"internal/proxy/compression_billing_test.go":                   5,
	"internal/proxy/cost_wiring_realpg_test.go":                    1,
	"internal/proxy/lxc_estimate_unknown_model_test.go":            1,
	"internal/proxy/lxc_gate.go":                                   1,
	"internal/proxy/pattern_capture_test.go":                       1,
	"internal/proxy/pattern_earn_test.go":                          3,
	"internal/proxy/proxy.go":                                      1,
	"internal/proxy/unbacked_credit_royalty_realpg_test.go":        1,
	"internal/workspace/attribution_isolation_integration_test.go": 2,
	"internal/workspace/compression_policy.go":                     5,
	"internal/worktier/worktier.go":                                1,
}

// ⚠ AND THE CITATIONS THIS GUARD CANNOT CHECK ARE COUNTED TOO, RATHER THAN DROPPED.
// A census that silently excludes what it cannot resolve reports a smaller problem
// than the one that exists. Five citations name no single file in this tree:
//
//   - a bare basename with several matches — `config.go`, `main.go` — which is WORSE
//     than a decayed line number, because it does not even name a file. Nothing here
//     can tell which config.go line 134 was;
//   - a path in ANOTHER repository (talyvor-track's lensintegration client), which is
//     #153's finding proper: unverifiable from inside one repo BY CONSTRUCTION.
//
// Pinned so the unverifiable population cannot grow quietly either.
var unresolvableCitationCensus = map[string]int{
	"cmd/lens/compose_env_reach_test.go":             1,
	"cmd/lens/provision_handler_test.go":             1,
	"internal/api/track_read_credential_test.go":     2,
	"internal/povi/node_harness_integration_test.go": 1,
}

func TestPointerAudit_TheLineCitationCensusIsPinned(t *testing.T) {
	root := repoRoot(t)
	files := goFiles(t, root)

	measured := map[string]int{}
	total := 0
	for _, c := range scan(t, root, files) {
		if c.symbol != "" || c.resolved == "" {
			continue // symbol citations are rule A's; cross-repo ones are uncheckable here
		}
		measured[c.from]++
		total++
	}

	for from, want := range lineCitationCensus {
		if got := measured[from]; got != want {
			t.Errorf("%s: %d line citations into this tree, census says %d. "+
				"Adding one is a NEW pointer that will decay; removing one means the pin comes down "+
				"in the same diff that converts it", from, got, want)
		}
	}
	for from, got := range measured {
		if _, ok := lineCitationCensus[from]; !ok {
			t.Errorf("%s carries %d line citation(s) and is not in the census — a new citing file "+
				"is a new pointer, not a new entry to add quietly", from, got)
		}
	}

	// ── RULE C, THE FLOOR ── the scanner must have READ something. A blinded scan
	// satisfies every comparison above by measuring an empty tree, and that is the
	// failure this whole file is about: an instrument that reads nothing reports a
	// clean product. #153's D3 in one assertion.
	if total < 25 {
		t.Errorf("the scan found only %d line citations across the tree; it is reading almost "+
			"nothing and every count above passed for free", total)
	}
	if n := len(files); n < 100 {
		t.Errorf("the walk found only %d .go files; the census is over a tree that is not this one", n)
	}
}

// ── RULE E — the citations this guard CANNOT check are counted too ──────────
//
// Without this rule the census above is flattering by construction: every
// unresolvable citation is skipped by its own `continue`, so the population that
// is hardest to verify is the one the number does not mention.
func TestPointerAudit_TheUnresolvableCitationCensusIsPinned(t *testing.T) {
	root := repoRoot(t)
	files := goFiles(t, root)

	measured := map[string]int{}
	total := 0
	for _, c := range scan(t, root, files) {
		if c.symbol != "" || c.resolved != "" {
			continue
		}
		measured[c.from]++
		total++
	}

	for from, want := range unresolvableCitationCensus {
		if got := measured[from]; got != want {
			t.Errorf("%s: %d citations naming no single file in this tree, census says %d", from, got, want)
		}
	}
	for from, got := range measured {
		if _, ok := unresolvableCitationCensus[from]; !ok {
			t.Errorf("%s carries %d citation(s) that name no single file in this tree and is not in "+
				"the census — an unverifiable pointer is the worst kind to add quietly", from, got)
		}
	}
	// The floor, for the same reason rule C has one: this rule's loops are also
	// satisfied by a scan that found nothing.
	if total < 4 {
		t.Errorf("the scan found only %d unresolvable citations; it is reading almost nothing", total)
	}
}

// ── RULE D — a cited line must exist ────────────────────────────────────────
//
// The weakest rule and the only one that is machine-decidable about a LINE: it
// cannot tell a pointer that moved from one that did not, only one that now
// points past the end of the file. It is here because that is the terminal form
// of the decay, it costs one stat per citation, and its positive control proves
// it can fire — which is the whole reason to keep a rule that is green today.
func TestPointerAudit_EveryCitedLineExists(t *testing.T) {
	root := repoRoot(t)
	files := goFiles(t, root)
	lines := map[string]int{}
	checked := 0

	for _, c := range scan(t, root, files) {
		if c.symbol != "" || c.resolved == "" {
			continue
		}
		n, ok := lines[c.resolved]
		if !ok {
			b, err := os.ReadFile(filepath.Join(root, c.resolved))
			if err != nil {
				t.Fatalf("read %s: %v", c.resolved, err)
			}
			n = strings.Count(string(b), "\n") + 1
			lines[c.resolved] = n
		}
		checked++
		if c.lineNo > n {
			t.Errorf("%s:%d cites %s but %s has %d lines — the pointer is past the end of the file",
				c.from, c.line, c.raw, c.resolved, n)
		}
	}
	if checked < 25 {
		t.Errorf("only %d citations were checked; this rule is reading almost nothing", checked)
	}
}

// A readable dump of the census, so a failure above can be diagnosed without
// re-deriving the scan by hand. Never an assertion.
func TestPointerAudit_Dump(t *testing.T) {
	if !testing.Verbose() {
		t.Skip("verbose only — this is a report, not a rule")
	}
	root := repoRoot(t)
	files := goFiles(t, root)
	var rows []string
	for _, c := range scan(t, root, files) {
		kind := "line"
		if c.symbol != "" {
			kind = "symbol"
		}
		rows = append(rows, fmt.Sprintf("%-6s %-60s %s -> %s", kind, c.from, c.raw, c.resolved))
	}
	sort.Strings(rows)
	t.Log("\n" + strings.Join(rows, "\n"))
}
