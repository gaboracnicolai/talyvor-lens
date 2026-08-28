package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// admin_route_scan_test.go — the structural reader behind the admin-route classification.
//
// ⚠ WHY THIS EXISTS. TestClassificationMatchesTheGateAtEachRegistrationSite is the wiring
// proof: without it, moving a path between the classification tables "would change a
// comment and nothing else". It decided whether a route carries the operator-read gate
// with a strings.Contains over a THREE-LINE window starting at the path's line. Measured
// against the real guard by rewriting the /v1/admin/lxc/grant registration in main.go —
// a route classified "MINTS LXC. Moves money." and one of the six the brief names by hand
// (~/talyvor-queue/w61-adminclass-mutation-controls-h2r7.py):
//
//	widened to requireAdminOrOperatorRead on ONE line          → CAUGHT (positive control)
//	the same widening, gate three lines below the path         → MISSED
//	the same widening through `gate := requireAdminOrOperatorRead` → MISSED
//	registration deleted, path left behind in a comment        → MISSED (the ghost check
//	                                                             cannot see the route go)
//
// So an LXC-minting route becomes reachable by the operator READ credential and all five
// guards in that file stay green. Spreading a registration over several lines is ordinary
// gofmt-stable Go — main.go already registers /v1/admin/keel/findings across two.
//
// ⚠ THE SPLIT: the STRUCTURAL questions (is this a registration, does its handler argument
// apply the gate) go to go/parser; the questions genuinely ABOUT NAMES (which path is this,
// what the gate is called) stay strings.

const operatorReadGate = "requireAdminOrOperatorRead"

// adminRoute is one /v1/admin path literal passed to a call in main.go, with whether that
// call's arguments really apply the operator-read gate.
type adminRoute struct {
	path  string
	gated bool
	line  int
}

// scanAdminRoutes returns every /v1/admin route literal main.go passes to a call.
// Parse errors are RETURNED: a scan that enumerates nothing classifies nothing.
func scanAdminRoutes(filename string, src []byte) ([]adminRoute, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}

	// The gate can be bound to a local before use (`gate := requireAdminOrOperatorRead`),
	// so follow it through assignment to a fixed point rather than matching one spelling.
	aliases := map[string]bool{operatorReadGate: true}
	appliesGate := func(e ast.Expr) bool {
		found := false
		ast.Inspect(e, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && aliases[types.ExprString(c.Fun)] {
				found = true
			}
			return !found
		})
		return found
	}
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
				if sel, ok := r.(*ast.SelectorExpr); ok && aliases[types.ExprString(sel)] {
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

	var out []adminRoute
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		for i, a := range call.Args {
			lit := adminPathLiteral(a)
			if lit == nil {
				continue
			}
			p, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.HasPrefix(p, "/v1/admin") {
				continue
			}
			r := adminRoute{path: p, line: fset.Position(lit.Pos()).Line}
			for j, other := range call.Args {
				if j != i && appliesGate(other) {
					r.gated = true
				}
			}
			out = append(out, r)
		}
		return true
	})
	return out, nil
}

// heldMintTypesFromAST reads the four mint types the revocation routes are generated from,
// out of the loop's own []string literal, so the expansion cannot drift from the loop.
func heldMintTypesFromAST(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		rng, ok := n.(*ast.RangeStmt)
		if !ok || out != nil {
			return true
		}
		key, kok := rng.Value.(*ast.Ident)
		if !kok || key.Name != "mt" {
			return true
		}
		cl, ok := rng.X.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if at, ok := cl.Type.(*ast.ArrayType); !ok || types.ExprString(at.Elt) != "string" {
			return true
		}
		for _, e := range cl.Elts {
			if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					out = append(out, v)
				}
			}
		}
		return true
	})
	return out, nil
}

// adminPathLiteral returns the string literal an argument names a route with: either the
// argument itself, or — for the held-mints loop, whose path is built as
// `"/v1/admin/held-mints/" + mt + "/adjudicate"` — the LEFTMOST literal of the
// concatenation, which is the prefix the classification expands from. Recognising the
// concatenated form structurally is why the four loop-generated PAYOUT REVOCATION routes
// are enumerated at all: a scanner that only accepted a bare literal would classify them
// as non-existent, which is the failure the original file's own header warns about.
func adminPathLiteral(e ast.Expr) *ast.BasicLit {
	for {
		switch x := e.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				return x
			}
			return nil
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return nil
			}
			e = x.X
		default:
			return nil
		}
	}
}
