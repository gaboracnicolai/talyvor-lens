package main

import (
	"go/ast"
	"go/token"
	"testing"
)

// rotation_route_wiring_test.go — W1.9.1's routes, asserted on main.go itself.
//
// These four routes mint and revoke credentials. The hazard is not that one is missing — a missing
// route is loud the first time anybody calls it — but that one is mounted where
// workspaceIsolationMiddleware does not reach, which is silent and lets any authenticated caller
// revoke a key in a workspace they do not own. Only routes hung on the `authed` group get that
// middleware (main.go's authed.Use), and only routes carrying a {wsID} segment make it do anything.

// the four W1.9.1 routes and what each one does, so a failure names the consequence
var rotationRoutes = map[string]string{
	"/v1/workspaces/{wsID}/api-keys/{keyID}/rotate/begin":       "mints a replacement credential",
	"/v1/workspaces/{wsID}/key-rotations/{rotationID}":          "reports a rotation's state",
	"/v1/workspaces/{wsID}/key-rotations/{rotationID}/complete": "REVOKES the old key",
	"/v1/workspaces/{wsID}/key-rotations/{rotationID}/abandon":  "REVOKES the new key",
}

func TestRotationRoutes_AreMountedInTheIsolatedGroup(t *testing.T) {
	_, f := mainGoAST(t)

	// ⚠ THIS ROUTER HAS TWO REGISTRATION SHAPES AND THE FIRST DRAFT ONLY READ ONE, which the RW3
	// floor is what caught:
	//
	//	authed.Post("/v1/…", h)      — the router is the RECEIVER, the pattern is arg 0
	//	econ.get(authed, "/v1/…", h) — the router is arg 0, the pattern is arg 1
	//
	// Reading only the second found zero of four registrations while reporting two of them as
	// "not registered". A shape-reading guard that knows one shape is a guard that reports the
	// absence of everything written the other way.
	seen := map[string]string{} // pattern -> the identifier the route is hung on
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		var pattern, recv string
		switch sel.Sel.Name {
		case "Get", "Post", "Put", "Delete", "Patch", "Head":
			// receiver form: authed.Post("/v1/…", h)
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pattern = lit.Value[1 : len(lit.Value)-1]
			if id, ok := sel.X.(*ast.Ident); ok {
				recv = id.Name
			} else {
				recv = "<not an identifier>"
			}
		case "get", "post", "put", "del", "delete":
			// helper form: econ.get(authed, "/v1/…", h)
			if len(call.Args) < 2 {
				return true
			}
			lit, ok := call.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pattern = lit.Value[1 : len(lit.Value)-1]
			if id, ok := call.Args[0].(*ast.Ident); ok {
				recv = id.Name
			} else {
				recv = "<not an identifier>"
			}
		default:
			return true
		}
		if _, want := rotationRoutes[pattern]; want {
			seen[pattern] = recv
		}
		return true
	})

	for pattern, what := range rotationRoutes {
		recv, mounted := seen[pattern]
		if !mounted {
			t.Errorf("[RW1] main.go registers no route at %s (%s). The store can be perfect and the "+
				"operator still has no way to run a rotation.", pattern, what)
			continue
		}
		// ⚠ THE RECEIVER IS THE ASSERTION. `authed` is the only group that applies
		// workspaceIsolationMiddleware; anywhere else, a route that %s serves any workspace to any
		// authenticated caller.
		if recv != "authed" {
			t.Errorf("[RW2] %s (%s) is registered on %q, not on the `authed` group — the only one "+
				"that applies workspaceIsolationMiddleware. Mounted anywhere else this is a "+
				"cross-tenant credential operation.", pattern, what, recv)
		}
	}

	// FLOOR: if the walk matched nothing, every loop above is vacuous.
	if len(seen) == 0 {
		t.Fatalf("[RW3] the walk found none of the %d rotation routes — it is not reading the "+
			"registrations, and 'all mounted correctly' would mean nothing", len(rotationRoutes))
	}
}

// TestRotationRoutes_AllCarryAWorkspaceSegment — workspaceIsolationMiddleware reads
// chi.URLParam(r, "wsID") and SKIPS its check when that is empty. A rotation route without the
// segment is therefore mounted inside the isolated group and still isolated by nothing.
func TestRotationRoutes_AllCarryAWorkspaceSegment(t *testing.T) {
	for pattern, what := range rotationRoutes {
		if !contains(pattern, "{wsID}") {
			t.Errorf("[RW4] %s (%s) has no {wsID} segment, so workspaceIsolationMiddleware skips it "+
				"even though it is mounted in the isolated group", pattern, what)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
