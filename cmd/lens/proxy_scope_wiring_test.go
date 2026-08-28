package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Wiring guard: every LLM proxy route (the serving surface) must be registered
// THROUGH the proxy-scope guard. Until this PR all four scopes were dead —
// RequireScope had zero callers — so a key lacking the proxy scope could still
// drive the proxy. This test fails on main (routes are bare authed.Post) and
// passes once each proxy registration goes through auth.RequireScope(auth.ScopeProxy).
func TestProxyRoutesAreScopeGuarded(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}

	defined, derr := proxyScopeGuardDefined("main.go", src)
	if derr != nil {
		t.Fatalf("parse main.go: %v — a guard that cannot read the file it polices must say so", derr)
	}
	if !defined {
		t.Fatal("main.go does not CALL auth.RequireScope(auth.ScopeProxy) — the proxy-scope guard " +
			"is not defined. ⚠ Naming it in a comment is not defining it, and binding the " +
			"middleware to a different scope is not defining it either.")
	}

	routes, rerr := proxyRoutesIn("main.go", src)
	if rerr != nil {
		t.Fatalf("parse main.go: %v", rerr)
	}
	if len(routes) < 9 {
		t.Fatalf("expected >=9 proxy route registrations, found %d — did the proxy paths change? "+
			"(a commented-out registration is NOT counted here, which is the point: the old "+
			"line-based version counted one and so could not see a route being removed)", len(routes))
	}

	var unguarded []string
	for _, r := range routes {
		if !r.guarded {
			unguarded = append(unguarded, r.path)
		}
	}
	if len(unguarded) > 0 {
		t.Fatalf("these proxy routes are NOT scope-guarded (each must go through the proxy-scope "+
			"middleware):\n  %s", strings.Join(unguarded, "\n  "))
	}
	t.Logf("%d proxy route registrations, all scope-guarded", len(routes))
}

type proxyRoute struct {
	path    string
	guarded bool
}

// isProxyPath is the one question here that is genuinely about a STRING — is this route path part
// of the LLM proxy surface — so it stays a string test. The structural questions around it (is this
// a registration, does its middleware chain apply the guard, what is that guard BOUND to) are read
// from the AST.
func isProxyPath(p string) bool {
	return strings.HasPrefix(p, "/v1/proxy/") || p == "/oai/*" || p == "/anthropic/*"
}

// isRequireProxyScope reports whether e is a real call to auth.RequireScope(auth.ScopeProxy).
//
// ⚠ A CALL, NOT THE TEXT OF ONE. The same characters in a comment or a string are not a middleware.
func isRequireProxyScope(e ast.Expr) bool {
	ce, ok := e.(*ast.CallExpr)
	if !ok || len(ce.Args) != 1 {
		return false
	}
	fn, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok || fn.Sel.Name != "RequireScope" {
		return false
	}
	if pkg, ok := fn.X.(*ast.Ident); !ok || pkg.Name != "auth" {
		return false
	}
	arg, ok := ce.Args[0].(*ast.SelectorExpr)
	if !ok || arg.Sel.Name != "ScopeProxy" {
		return false
	}
	pkg, ok := arg.X.(*ast.Ident)
	return ok && pkg.Name == "auth"
}

// proxyScopeIdents returns the identifiers this file BINDS to the proxy-scope middleware.
//
// ⚠ THIS IS THE HALF A LINE SCAN CANNOT REACH. "there is an identifier called proxyScope on this
// line" and "that identifier is the PROXY-scope middleware" are different claims and only the
// second is the property. Binding it to auth.ScopeAnalytics instead would let an analytics-only key
// drive the billed LLM proxy, with every route line still reading exactly the same.
func proxyScopeIdents(f *ast.File) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range d.Lhs {
				if i < len(d.Rhs) && isRequireProxyScope(d.Rhs[i]) {
					if id, ok := lhs.(*ast.Ident); ok {
						out[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for i, name := range d.Names {
				if i < len(d.Values) && isRequireProxyScope(d.Values[i]) {
					out[name.Name] = true
				}
			}
		}
		return true
	})
	return out
}

// chainApplies reports whether a route's receiver chain routes it through the proxy-scope guard,
// either by an identifier bound to it or by applying it inline.
func chainApplies(recv ast.Expr, binds map[string]bool) bool {
	found := false
	ast.Inspect(recv, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := ce.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "With" {
			return true
		}
		for _, a := range ce.Args {
			if id, ok := a.(*ast.Ident); ok && binds[id.Name] {
				found = true
			}
			if isRequireProxyScope(a) {
				found = true
			}
		}
		return true
	})
	return found
}

// proxyRoutesIn returns every LLM proxy route registration in src and whether its middleware chain
// applies the proxy-scope guard.
//
// ⚠ A PARSE ERROR IS RETURNED, NEVER SWALLOWED: a scanner that returns nothing finds no unguarded
// routes and this guard would pass over a file it could not read.
func proxyRoutesIn(filename string, src []byte) ([]proxyRoute, error) {
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		return nil, err
	}
	binds := proxyScopeIdents(f)
	var out []proxyRoute
	ast.Inspect(f, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok || len(ce.Args) == 0 {
			return true
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Post" {
			return true
		}
		lit, ok := ce.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		path, uerr := strconv.Unquote(lit.Value)
		if uerr != nil || !isProxyPath(path) {
			return true
		}
		out = append(out, proxyRoute{path: path, guarded: chainApplies(sel.X, binds)})
		return true
	})
	return out, nil
}

// proxyScopeGuardDefined reports whether src really CALLS auth.RequireScope(auth.ScopeProxy).
func proxyScopeGuardDefined(filename string, src []byte) (bool, error) {
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		return false, err
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok && isRequireProxyScope(e) {
			found = true
		}
		return true
	})
	return found, nil
}

// ── the guard reads REGISTRATIONS, not lines ────────────────────────────────────────────────────

// ⚠ THIS IS AN AUTHZ GUARD AND IT COULD BE TURNED OFF BY A TRAILING COMMENT. Measured 2026-08-28
// (tab-q6d3) against the real guard, by editing cmd/lens/main.go one arm at a time and restoring it
// sha256-verified between runs.
//
// ⚠⚠ SAID FIRST: ALL NINE REAL PROXY ROUTES ARE CORRECTLY GUARDED AND THIS FIXES NO LIVE HOLE. The
// defect is in the instrument, and it is that the rule was "the LINE containing the registration
// must also contain the text `proxyScope`".
//
//	A1 an unguarded 10th route, no comment          -> CAUGHT   (the positive control)
//	A2 the same route + `// proxyScope is applied by the parent router`  -> MISSED
//	A3 the same route + `// guarded upstream by RequireScope(auth.ScopeProxy)` -> MISSED
//	A4 the same route + `// see docs/proxyScope-notes.md`                -> MISSED
//
// A note next to a route — including one that only names a FILE — makes an unguarded LLM proxy
// route pass the guard written to stop exactly that. The header of this file says what is at stake:
// "a key lacking the proxy scope could still drive the proxy".
//
//	A5 a fully commented-out route, no scope named  -> CAUGHT, reported as an unguarded route
//	A6 comment OUT a real guarded route (9 live -> 8) -> MISSED: the >=9 floor counts the dead line,
//	   so the check that exists to notice "did the proxy paths change?" cannot see one removed.
//
// ⚠⚠⚠ AND THE ARM THAT DECIDED THE SHAPE OF THE FIX. A7: bind the middleware to the WRONG SCOPE and
// leave the right one in a comment —
//
//	// the proxy surface is gated by auth.RequireScope(auth.ScopeProxy)
//	proxyScope := auth.RequireScope(auth.ScopeAdmin)
//
// -> MISSED. Every proxy route is then gated on a different scope and this guard still reports the
// proxy surface as proxy-scope guarded. The old file-level check was a Contains over the whole text,
// so the comment satisfied it, and the per-line check only ever looked for the IDENTIFIER, never at
// what it was bound to. ⚠ NOT OVERQUOTED: ScopeAdmin is NARROWER, so A7 as written locks users out
// rather than letting them in. The point is that the guard cannot tell WHICH scope — bind it to
// auth.ScopeAnalytics and an analytics-only key drives the billed LLM proxy, with this guard green.
//
// So the registration and the binding are both read from the AST now: a route is guarded when its
// middleware chain applies an identifier that this file BINDS to auth.RequireScope(auth.ScopeProxy).
func TestProxyScopeScannerReadsRegistrationsNotLines(t *testing.T) {
	const head = "package main\n\nfunc reg() {\n\tproxyScope := auth.RequireScope(auth.ScopeProxy)\n\t_ = proxyScope\n"
	const tail = "}\n"

	cases := []struct {
		name        string
		body        string
		wantRoutes  int
		wantGuarded int
	}{
		{"a guarded route", "\tauthed.With(proxyScope).Post(\"/v1/proxy/openai/*\", h)\n", 1, 1},
		{"a guarded legacy route", "\tauthed.With(proxyScope).Post(\"/oai/*\", h)\n", 1, 1},
		{"an unguarded route", "\tauthed.Post(\"/v1/proxy/openai/*\", h)\n", 1, 0},

		// ⚠ THE FOUR ARMS MEASURED ON THE REAL GUARD. Each is an unguarded route that the
		// line-based rule accepted.
		{"unguarded + a trailing comment naming the identifier",
			"\tauthed.Post(\"/v1/proxy/openai/*\", h) // proxyScope is applied by the parent router\n", 1, 0},
		{"unguarded + a trailing comment naming the middleware",
			"\tauthed.Post(\"/v1/proxy/openai/*\", h) // guarded by RequireScope(auth.ScopeProxy)\n", 1, 0},
		{"unguarded + a comment naming only a FILE",
			"\tauthed.Post(\"/v1/proxy/openai/*\", h) // see docs/proxyScope-notes.md\n", 1, 0},
		{"a commented-out route is not a route",
			"\t// authed.Post(\"/v1/proxy/legacy/*\", h)\n", 0, 0},

		// ⚠ AND THE ROWS THAT MUST NOT REGRESS: a registration split over lines is still one
		// registration, and the guard on it still counts.
		{"a multi-line guarded registration",
			"\tauthed.With(proxyScope).\n\t\tPost(\"/v1/proxy/openai/*\", h)\n", 1, 1},
		{"a multi-line unguarded registration",
			"\tauthed.\n\t\tPost(\"/v1/proxy/openai/*\", h)\n", 1, 0},
		{"a chained With still guards",
			"\tauthed.With(other).With(proxyScope).Post(\"/v1/proxy/openai/*\", h)\n", 1, 1},

		// ⚠ THIS ROW EXISTS BECAUSE A MUTATION CONTROL CAME BACK VOID. Removing the `.With` test
		// from chainApplies changed no verdict in this table, which means the rule "middleware is
		// applied by .With" was asserted by nothing. Passing the guard to some OTHER call in the
		// chain does not apply it as middleware, and without this row the scanner could be relaxed
		// to accept that with every test still green.
		{"passing the guard to a call that is not .With does not apply it",
			"\tauthed.Handle(proxyScope).Post(\"/v1/proxy/openai/*\", h)\n", 1, 0},
		{"a non-proxy route is not counted at all",
			"\tauthed.Post(\"/v1/workspaces\", h)\n", 0, 0},
		{"a string that merely contains a proxy path is not a registration",
			"\tlog(\"registering .Post(\\\"/v1/proxy/openai/*\\\")\")\n", 0, 0},
	}

	for _, tc := range cases {
		got, err := proxyRoutesIn("probe.go", []byte(head+tc.body+tail))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		guarded := 0
		for _, r := range got {
			if r.guarded {
				guarded++
			}
		}
		if len(got) != tc.wantRoutes || guarded != tc.wantGuarded {
			t.Errorf("%s:\n  want %d route(s), %d guarded\n  got  %d route(s), %d guarded  %+v",
				tc.name, tc.wantRoutes, tc.wantGuarded, len(got), guarded, got)
		}
	}
}

// ⚠ THE BINDING IS THE HALF A LINE SCAN CANNOT REACH AT ALL. "There is an identifier called
// proxyScope on this line" and "that identifier is the PROXY-scope middleware" are different
// claims, and only the second one is the property.
func TestProxyScopeGuardDefinitionIsRead(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"bound to the proxy scope",
			"package main\nfunc f() { proxyScope := auth.RequireScope(auth.ScopeProxy); _ = proxyScope }\n", true},
		{"bound to a DIFFERENT scope, with the right one in a comment",
			"package main\n// gated by auth.RequireScope(auth.ScopeProxy)\n" +
				"func f() { proxyScope := auth.RequireScope(auth.ScopeAdmin); _ = proxyScope }\n", false},
		{"named only in a comment",
			"package main\n// auth.RequireScope(auth.ScopeProxy)\nfunc f() {}\n", false},
		{"named only in a string",
			"package main\nconst s = \"auth.RequireScope(auth.ScopeProxy)\"\n", false},
		{"a different middleware entirely",
			"package main\nfunc f() { x := auth.RequireScope(auth.ScopeMint); _ = x }\n", false},
	}
	for _, tc := range cases {
		got, err := proxyScopeGuardDefined("probe.go", []byte(tc.src))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: want %v, got %v", tc.name, tc.want, got)
		}
	}
}

// ⚠ AND BOTH MUST FAIL LOUDLY ON SOURCE THEY CANNOT READ. A scanner that returns nothing finds no
// unguarded routes; a definition check that returns false on a parse error fails on correct code.
func TestProxyScopeScannersRefuseSourceTheyCannotParse(t *testing.T) {
	bad := []byte("package main\nfunc f() { authed.Post( {{{\n")
	if _, err := proxyRoutesIn("broken.go", bad); err == nil {
		t.Error("the route scanner read unparseable source and reported no routes")
	}
	if _, err := proxyScopeGuardDefined("broken.go", bad); err == nil {
		t.Error("the definition check read unparseable source and returned an answer")
	}
}
