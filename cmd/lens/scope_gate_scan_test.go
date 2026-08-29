package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// scope_gate_scan_test.go — the structural reader for "where is auth.RequireScope called".
//
// ⚠ WHY THIS EXISTS. TestRequireScopeHasExactlyOneCallSite is the authz census the W4.6.1
// escalation measurement rests on: six scope constants are declared and RequireScope is called
// with exactly one of them, so `analytics` and `keys` gate nothing. Its own message states the
// stake in both directions — "If a scope gate was ADDED, that is a real narrowing… If one was
// REMOVED, a scope boundary just disappeared silently." It counted LINES of main.go containing
// `auth.RequireScope(`, skipping only lines starting with `//`. Measured by adding a second real
// scope gate (~/talyvor-queue/w61-sessioncred-controls-h2r7.py):
//
//	a second gate written the same way              → CAUGHT (the positive control)
//	`rs := auth.RequireScope` then `rs(ScopeAdmin)` → MISSED
//	a second gate in ANOTHER FILE of package main   → MISSED
//
// The second miss is the larger one, and it is a number: cmd/lens has 57 non-test .go files and
// the census read ONE of them while making a claim about the binary. There is one call site today
// — verified across all 57 — so nothing is presently wrong; what was missing is that a gate added
// in any of the other 56, or through an alias in main.go itself, would not move the count.
//
// ⚠ THE SPLIT: the STRUCTURAL question (is this a call to RequireScope, however it was bound)
// goes to go/parser; the questions genuinely ABOUT NAMES (which function, which scope constant)
// stay strings. Comments and string literals are not in the AST, so the comment-skipping the line
// rule had to do by hand is now free — and it no longer mistakes a trailing comment, a block
// comment or a string literal for a call, which the line rule did.

const requireScopeFunc = "auth.RequireScope"

// scopeGateSite is one call to auth.RequireScope, directly or through a local alias.
type scopeGateSite struct {
	file  string
	line  int
	scope string // the rendered first argument, e.g. "auth.ScopeProxy"
	alias string // the identifier it was called through, or "" when called directly
}

// scanScopeGates parses every non-test .go file in dir and returns each RequireScope call site
// plus the files it read. Parse errors are RETURNED: a file this scan cannot read is a file whose
// scope gates are uncounted, and the census would report a smaller number rather than an error.
func scanScopeGates(dir string) (sites []scopeGateSite, files []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		files = append(files, n)
	}
	sort.Strings(files)

	for _, name := range files {
		src, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			return nil, nil, rerr
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			return nil, nil, perr
		}

		// Follow `rs := auth.RequireScope` to a fixed point, so binding the gate to a local
		// before calling it does not hide the call.
		aliases := map[string]bool{}
		for grew := true; grew; {
			grew = false
			ast.Inspect(f, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				carries := false
				for _, r := range as.Rhs {
					switch x := r.(type) {
					case *ast.SelectorExpr:
						if types.ExprString(x) == requireScopeFunc {
							carries = true
						}
					case *ast.Ident:
						if aliases[x.Name] {
							carries = true
						}
					}
				}
				if !carries {
					return true
				}
				for _, l := range as.Lhs {
					if id, ok := l.(*ast.Ident); ok && id.Name != "_" && !aliases[id.Name] {
						aliases[id.Name] = true
						grew = true
					}
				}
				return true
			})
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			s := scopeGateSite{file: name, line: fset.Position(call.Pos()).Line}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if types.ExprString(fn) != requireScopeFunc {
					return true
				}
			case *ast.Ident:
				if !aliases[fn.Name] {
					return true
				}
				s.alias = fn.Name
			default:
				return true
			}
			if len(call.Args) > 0 {
				s.scope = types.ExprString(call.Args[0])
			}
			sites = append(sites, s)
			return true
		})
	}
	return sites, files, nil
}
