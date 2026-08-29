package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
)

// go_statement_scan_test.go — the structural reader for main.go's background goroutines.
//
// ⚠ WHY THIS EXISTS, AND THE SECOND DEFECT IS WORSE THAN THE FIRST. The classification census
// enumerated goroutines with `^\s*go ` per LINE and identified each by a substring of that line.
// Measured (~/talyvor-queue/w61-goroutine-census-controls-h2r7.py):
//
//	an unclassified `go logger.Info(…)` at the start of its line  → CAUGHT (positive control)
//	the same inside `if cond { go … }` written on ONE line        → MISSED, and not as
//	  "unclassified" — the goroutine never entered the population at all
//	the same inside `for … { go … }` on one line                  → MISSED, the same way
//	ANY new anonymous `go func() {` goroutine                     → silently CLASSIFIED as
//	  "stranded reservation sweep"
//
// The last one is the serious one. That entry's own reason begins "IDEMPOTENT BY ROW LOCK, and
// this one was checked hardest because it MOVES MONEY" — and its needle is
// `"go func() {\n\t\t\tt := time.NewTicker(2 * time.Minute)"`, of which the matcher only ever
// compares the FIRST LINE, `go func() {`. Every anonymous goroutine in the file begins with those
// characters. So a new one inherits a money-path reason nobody wrote for it, and the guard whose
// whole purpose is "a goroutine nobody has decided about" reports it as decided.
//
// (The multi-line needles were decorative for the same reason: the matcher splits on "\n" and
// uses index 0. The second line was never compared.)
//
// ⚠ THE SPLIT: WHICH goroutines exist and WHAT each one calls are structural and come from
// go/parser — `*ast.GoStmt` is a `go` statement wherever it sits on a line. Which callee names
// mean which classification stays a table of strings, because that is what it is.

// goStmtSite is one `go` statement in main.go.
type goStmtSite struct {
	callee  string   // the rendered callee of a named call; "" for a func literal
	jobName string   // haComps.leader.Run's job name; "" otherwise
	calls   []string // every callee invoked inside the statement, closure bodies included
	line    int
}

// matches reports whether needle identifies this goroutine: it is the callee, or it is called
// inside it. A closure is identified by what it DOES, never by a comment or by the `func() {`
// prefix every closure shares.
func (g goStmtSite) matches(needle string) bool {
	if g.callee == needle {
		return true
	}
	for _, c := range g.calls {
		if c == needle {
			return true
		}
	}
	return false
}

// scanGoStatements returns every `go` statement in src. Parse errors are RETURNED: a scan that
// enumerates no goroutines reports every goroutine as classified.
func scanGoStatements(filename string, src []byte) ([]goStmtSite, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var out []goStmtSite
	ast.Inspect(f, func(n ast.Node) bool {
		gs, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		site := goStmtSite{line: fset.Position(gs.Pos()).Line}
		if _, isLit := gs.Call.Fun.(*ast.FuncLit); !isLit {
			site.callee = types.ExprString(gs.Call.Fun)
		}
		if site.callee == leaderRunSelector && len(gs.Call.Args) >= 2 {
			if lit, ok := gs.Call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, uerr := strconv.Unquote(lit.Value); uerr == nil {
					site.jobName = v
				}
			}
		}
		seen := map[string]bool{}
		ast.Inspect(gs.Call, func(m ast.Node) bool {
			if c, ok := m.(*ast.CallExpr); ok {
				if name := types.ExprString(c.Fun); name != "" && !seen[name] {
					seen[name] = true
					site.calls = append(site.calls, name)
				}
			}
			return true
		})
		out = append(out, site)
		return true
	})
	return out, nil
}
