package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
)

// replica_taint_scan_test.go — the structural reader behind the read-replica invariant.
//
// ⚠ WHY THIS EXISTS. TestReadReplicaWiring_MoneyAuthzNeverReceiveReplica is described in
// its own comment as THE invariant: money/authz/tx constructors must PHYSICALLY NEVER
// receive the replica pool. It enforced that by scanning for a money constructor and a
// replica reference ON THE SAME LINE. Measured against the real guard by rewriting
// `keyStore := auth.New(pool)` in main.go one arm at a time
// (~/talyvor-queue/w61-replica-invariant-controls-h2r7.py):
//
//	auth.New(replicaPool)                          one line  → CAUGHT (positive control)
//	auth.New(dbrouting.ReadPool(pool, replicaPool)) one line  → CAUGHT (positive control)
//	auth.New(\n\t\treplicaPool,\n\t)                          → MISSED
//	authPool := replicaPool; auth.New(authPool)               → MISSED
//
// `auth.New` is the revoked-key authz store, so a MISS means a revoked API key keeps
// authenticating for the length of the replica lag — and the miss is produced by a LINE
// BREAK, in a file that already writes 25 constructor calls that way, and by a one-line
// alias. Neither is exotic; both survive gofmt.
//
// The ReadPool-shaped multi-line arm was caught, but by the SIBLING count guard
// (ExactlySixAnalyticsReaders going 6→7), not by the invariant — an accidental backstop
// that vanishes the moment the violation names `replicaPool` directly, which is the
// cheaper way to write it.
//
// ⚠ THE SPLIT IS #515's AND #516's: the STRUCTURAL question (does this call receive the
// replica, however it was spelled or spaced) goes to go/parser; the questions genuinely
// ABOUT NAMES (which constructors are money/authz, what the pool variable is called)
// stay strings.

// replicaPoolIdent is the variable main.go binds the replica pool to, and readPoolFunc is
// the router that hands it out. Both are NAMES, matched against rendered AST expressions.
const (
	replicaPoolIdent = "replicaPool"
	readPoolFunc     = "dbrouting.ReadPool"
)

// ctorCall is one constructor call in main.go, with the argument that carries the replica
// pool if any does.
type ctorCall struct {
	name       string // the rendered callee, e.g. "auth.New"
	pos        string // where it is, as "<file>:<line>" — not a pointer INTO this tree, so it does not decay
	replicaArg string // the rendered argument that reaches the replica, or "" if none does
}

// replicaScan is what one parse of main.go yields.
type replicaScan struct {
	// tainted is every identifier that holds the replica pool, seeded with replicaPool and
	// closed over assignment: `authPool := replicaPool` taints authPool too.
	tainted map[string]bool
	// readPoolCalls counts real dbrouting.ReadPool CALLS — not occurrences of the text, so
	// a commented-out wiring line does not restore a reader that was removed.
	readPoolCalls int
	calls         []ctorCall
}

// scanReplicaWiring parses src and answers both questions the read-routing guards ask.
// Parse errors are RETURNED: a scan that reports "no violations" because the file did not
// parse is a guard that cannot fail.
func scanReplicaWiring(filename string, src []byte) (*replicaScan, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	s := &replicaScan{tainted: map[string]bool{replicaPoolIdent: true}}

	// Close the taint set over assignments and var declarations until it stops growing, so
	// an alias chain of any length is followed rather than only a direct reference.
	for grew := true; grew; {
		grew = false
		ast.Inspect(f, func(n ast.Node) bool {
			var lhs, rhs []ast.Expr
			switch d := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = d.Lhs, d.Rhs
			case *ast.ValueSpec:
				for _, id := range d.Names {
					lhs = append(lhs, id)
				}
				rhs = d.Values
			default:
				return true
			}
			carries := false
			for _, e := range rhs {
				if s.reachesReplica(e) {
					carries = true
					break
				}
			}
			if !carries {
				return true
			}
			for _, e := range lhs {
				if id, ok := e.(*ast.Ident); ok && id.Name != "_" && !s.tainted[id.Name] {
					s.tainted[id.Name] = true
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
		if types.ExprString(call.Fun) == readPoolFunc {
			s.readPoolCalls++
		}
		c := ctorCall{name: types.ExprString(call.Fun), pos: fmt.Sprintf("%s:%d", filename, fset.Position(call.Pos()).Line)}
		for _, a := range call.Args {
			if s.reachesReplica(a) {
				c.replicaArg = types.ExprString(a)
				break
			}
		}
		s.calls = append(s.calls, c)
		return true
	})
	return s, nil
}

// reachesReplica reports whether evaluating e can yield the replica pool: it names a
// tainted identifier, or it calls the read router.
func (s *replicaScan) reachesReplica(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if s.tainted[x.Name] {
				found = true
			}
		case *ast.CallExpr:
			if types.ExprString(x.Fun) == readPoolFunc {
				found = true
			}
		}
		return !found
	})
	return found
}

// wraps reports whether some call to ctor takes arg directly — the shape
// `forecast.NewStore(dbrouting.ReadPool(pool, replicaPool))` asserts as a real nested
// CALL rather than as a substring of the file.
func (s *replicaScan) wraps(outer, innerFunc string, src []byte, filename string) (bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return false, err
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || types.ExprString(call.Fun) != outer {
			return true
		}
		for _, a := range call.Args {
			if inner, ok := a.(*ast.CallExpr); ok && types.ExprString(inner.Fun) == innerFunc {
				found = true
			}
		}
		return true
	})
	return found, nil
}
