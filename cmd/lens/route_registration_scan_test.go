package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// route_registration_scan_test.go — the structural reader for "what does main.go register,
// and on which router".
//
// ⚠ WHY THIS EXISTS. TestEconomyKillSwitch_ManifestCoverage is the forgotten-gate tripwire
// for the U3 master economy kill switch, and its own comment states the contract: "a bare
// router.Verb(\"/v1/economy-path\" fails the build". A BARE economy route is one the master
// switch cannot withhold — it serves with LENS_ECONOMY_ENABLED=false. It enforced that with
//
//	`\b(?:authed|pub|r)\.(?:Get|Post|Delete)\("([^"]+)"`
//
// Measured by adding one bare economy route to main.go per arm, restored sha256-verified
// (~/talyvor-queue/w61-manifestcov-mutation-controls-h2r7.py):
//
//	authed.Get("/v1/economy/probe", h)     one line  → CAUGHT (the positive control)
//	the same registration split across lines         → MISSED
//	authed.Put("/v1/economy/probe", h)               → MISSED
//	authed.Patch("/v1/economy/probe", h)             → MISSED
//	r.Handle("/v1/economy/probe", h)                 → MISSED — and r is the UNAUTHENTICATED
//	                                                    root router, so that arm is a bare
//	                                                    economy route with no auth at all
//
// Those are not hypothetical verbs: main.go registers 10 routes with authed.Put, 9 with
// r.Handle and 2 with authed.Patch — 21 real sites the tripwire could not see, by
// construction, while claiming to fail the build on any of them.
//
// ⚠ AND THE COVERAGE PROBLEM IS THE REAL ONE, so it is guarded rather than re-hardcoded:
// routerVerbs below is a LIST, and a list goes stale the moment chi's Router is used a new
// way. TestRouterVerbCoverage_NoRegistrationShapeIsUnseen fails if main.go calls any method
// on a router that this scanner neither registers nor knows to be a non-registering one.
// A silent cap on coverage is what produced the defect above.

// routerVerbs maps a chi Router registration method to the index of its PATH argument.
var routerVerbs = map[string]int{
	"Get": 0, "Post": 0, "Put": 0, "Patch": 0, "Delete": 0,
	"Head": 0, "Options": 0, "Connect": 0, "Trace": 0,
	"Handle": 0, "HandleFunc": 0, "Mount": 0,
	"Method": 1, "MethodFunc": 1,
}

// routerNonRegistering are the chi Router methods that do NOT bind a path. They are listed
// so the coverage guard can tell "a shape I know is not a registration" from "a shape I have
// never seen", which is the distinction the regex could not make.
var routerNonRegistering = map[string]bool{
	"Use": true, "With": true, "Group": true, "Route": true,
	"NotFound": true, "MethodNotAllowed": true, "ServeHTTP": true, "Routes": true,
	"Match": true, "Find": true, "Middlewares": true,
}

// routeReg is one path binding in main.go.
type routeReg struct {
	receiver string // the router expression, e.g. "authed" or "r"
	verb     string
	path     string
	line     int
	// handler is the rendered argument that follows the path — what actually serves the route,
	// gate wrapper and all.
	handler string
	// conds are the `if` conditions that govern the registration, outermost first, rendered from
	// the AST. An else-branch renders `!(cond)`.
	conds []string
	// handlerExpr is the unrendered form of handler, kept so wrapsCall can ask a structural
	// question about it rather than a textual one.
	handlerExpr ast.Expr
}

// wrapsCall reports whether the route's handler expression applies fn — `requireAdmin(...)`
// wrapping the real handler, at any depth. It is a question about a CALL, so a comment or a
// string naming fn cannot answer it.
func (r routeReg) wrapsCall(fn string) bool {
	if r.handlerExpr == nil {
		return false
	}
	found := false
	ast.Inspect(r.handlerExpr, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && types.ExprString(c.Fun) == fn {
			found = true
		}
		return !found
	})
	return found
}

// gatedOn reports whether cond is among the `if` conditions enclosing the registration.
func (r routeReg) gatedOn(cond string) bool {
	for _, c := range r.conds {
		if c == cond {
			return true
		}
	}
	return false
}

// scanRouteRegistrations returns every path main.go binds on a router, the router expressions
// it found, and any method called on one of those routers that is neither a known
// registration nor a known non-registration. Parse errors are RETURNED: a scan that finds no
// registrations reports no bare economy routes.
func scanRouteRegistrations(filename string, src []byte) (regs []routeReg, routers []string, unknown map[string][]string, err error) {
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, filename, src, 0)
	if perr != nil {
		return nil, nil, nil, perr
	}

	isRouter := map[string]bool{}
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		idx, known := routerVerbs[sel.Sel.Name]
		if !known || idx >= len(call.Args) {
			return true
		}
		lit, ok := call.Args[idx].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		p, uerr := strconv.Unquote(lit.Value)
		if uerr != nil || !strings.HasPrefix(p, "/") {
			return true
		}
		recv := types.ExprString(sel.X)
		isRouter[routerBase(recv)] = true
		reg := routeReg{receiver: recv, verb: sel.Sel.Name, path: p, line: fset.Position(lit.Pos()).Line,
			conds: enclosingIfConds(stack)}
		if idx+1 < len(call.Args) {
			reg.handlerExpr = call.Args[idx+1]
			reg.handler = types.ExprString(call.Args[idx+1])
		}
		regs = append(regs, reg)
		return true
	})

	unknown = map[string][]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base := routerBase(types.ExprString(sel.X))
		if !isRouter[base] {
			return true
		}
		m := sel.Sel.Name
		if _, ok := routerVerbs[m]; ok || routerNonRegistering[m] {
			return true
		}
		unknown[m] = append(unknown[m], base)
		return true
	})

	for k := range isRouter {
		routers = append(routers, k)
	}
	return regs, routers, unknown, nil
}

// routerBase strips a chained call off a router expression so `authed.With(x)` and `authed`
// are the same router. Without this a middleware chain would read as an unrelated receiver
// and its registrations would fall outside the census.
func routerBase(expr string) string {
	if i := strings.IndexAny(expr, ".("); i > 0 {
		return expr[:i]
	}
	return expr
}

// enclosingIfConds renders the condition of every `if` whose BODY (or else-branch) contains the
// node. A call in the `if`'s own CONDITION is not governed by it and contributes nothing.
func enclosingIfConds(stack []ast.Node) []string {
	var conds []string
	for i := 0; i+1 < len(stack); i++ {
		ifs, ok := stack[i].(*ast.IfStmt)
		if !ok {
			continue
		}
		switch child := stack[i+1]; {
		case ast.Node(ifs.Body) == child:
			conds = append(conds, types.ExprString(ifs.Cond))
		case ifs.Else != nil && ifs.Else == child:
			conds = append(conds, "!("+types.ExprString(ifs.Cond)+")")
		}
	}
	return conds
}
