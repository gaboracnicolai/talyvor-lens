package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
)

// scoped_handler_scan_test.go — the structural reader for "this route goes through the SCOPED
// handler, and nothing bypasses it".
//
// ⚠ WHY THIS EXISTS. Three files end with the same wiring assertion, and each says the same
// thing in its own words: the handler can be perfect and UNREACHED, and every other test in the
// file is then driving a handler the binary does not serve. All three asserted it with an
// exact-text strings.Contains for the registration, and forbade a bypass with strings.Count over
// raw source. Measured (~/talyvor-queue/w61-scopedhandler-controls-h2r7.py), whole package per arm:
//
//	POST /v1/prompts served by an UNSCOPED inline handler          → CAUGHT (positive control)
//	the same, scoped registration left in a COMMENT                → MISSED
//	the same for POST /v1/eval/cases                               → MISSED
//	the same for GET /v1/local/endpoints                           → MISSED
//	a bypass create through `pm := promptManager; pm.Create(…)`    → MISSED
//
// The first four mean the workspace scoping those handlers exist to apply is unreached while the
// guard says it is wired. The fifth is the bypass the Count rule was written to stop, walking
// past it through a one-line alias.
//
// ⚠ THE SPLIT: registration and handler come from scanRouteRegistrations (#524); the bypass
// question is a TAINT question — does any call to this method run on this object however it was
// named — and is answered here.

// callsOnAliasesOf returns the line of every call to <ident>.<method>, following aliases of ident
// through assignment to a fixed point. `pm := promptManager` then `pm.Create(…)` is the same call
// on the same object, and a rule that matches the receiver's spelling cannot see it.
func callsOnAliasesOf(filename string, src []byte, ident, method string) ([]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	aliases := map[string]bool{ident: true}
	for grew := true; grew; {
		grew = false
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			carries := false
			for _, r := range as.Rhs {
				if id, ok := r.(*ast.Ident); ok && aliases[id.Name] {
					carries = true
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
	var out []int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && aliases[id.Name] {
			out = append(out, fset.Position(call.Pos()).Line)
		}
		return true
	})
	return out, nil
}

// rawValueReachesResponse returns the line of every call to writeFunc whose arguments carry the
// result of <ident>.<method>() — directly, or through a variable assigned from it.
//
// ⚠ NOT "main.go never calls List()". It legitimately does, once, inside the `local_models`
// health check, which COUNTS endpoints and serves none. What must not exist is a call whose
// result is written to a RESPONSE — which is a question about where the VALUE goes, not about
// whether the call appears.
func rawValueReachesResponse(filename string, src []byte, ident, method, writeFunc string) ([]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	isRawCall := func(e ast.Expr) bool {
		found := false
		ast.Inspect(e, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == ident {
					found = true
				}
			}
			return !found
		})
		return found
	}
	tainted := map[string]bool{}
	for grew := true; grew; {
		grew = false
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			carries := false
			for _, r := range as.Rhs {
				if isRawCall(r) {
					carries = true
					continue
				}
				ast.Inspect(r, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && tainted[id.Name] {
						carries = true
					}
					return true
				})
			}
			if !carries {
				return true
			}
			for _, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name != "_" && !tainted[id.Name] {
					tainted[id.Name] = true
					grew = true
				}
			}
			return true
		})
	}
	var out []int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || types.ExprString(call.Fun) != writeFunc {
			return true
		}
		for _, a := range call.Args {
			carries := isRawCall(a)
			if !carries {
				ast.Inspect(a, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && tainted[id.Name] {
						carries = true
					}
					return true
				})
			}
			if carries {
				out = append(out, fset.Position(call.Pos()).Line)
				break
			}
		}
		return true
	})
	return out, nil
}
