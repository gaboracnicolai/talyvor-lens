package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// public_node_discovery_mount_test.go — the premise of the narrowed projection,
// read from the ROUTER rather than from a comment.
//
// publicInferenceNode / publicEmbeddingNode drop `url` and `workspace_id`
// BECAUSE the two routes that use them are unauthenticated. That "because" is
// the whole justification, and nothing in Go checks it: if someone later moves
// these routes behind auth.AuthMiddleware, the narrowing becomes an unexplained
// amputation of an authenticated API; if someone adds a THIRD anonymous route
// that marshals mining.InferenceNode directly, the narrowing is bypassed
// entirely. Both are silent today.
//
// ⚠ WHY THE INPUT IS main.go's SOURCE. The router is constructed inline inside
// run()'s full dependency graph (a live pool, redis, nats, ~40 constructors), so
// there is no seam a test can mount and chi.Walk is unreachable —
// admin_route_classification_test.go documents the same constraint for
// /v1/admin. Saying that plainly beats walking an empty router, which would pass
// while covering nothing.
//
// ⚠ WHY THE CHECK IS "NO AUTH MIDDLEWARE ON THE ENCLOSING GROUP" AND NOT "THE
// CLOSURE PARAMETER IS NAMED pub". A name is a convention, and renaming it would
// silently disarm this. The middleware chain is the thing that decides.

const (
	publicNodesPath      = `"/v1/nodes/available"`
	publicEmbedNodesPath = `"/v1/embedding-nodes/available"`
	authMiddlewareName   = "AuthMiddleware"
)

// routeMount is what the walker learned about one route-path literal.
type routeMount struct {
	line     int
	inGroup  bool     // the registration sits inside a .Group(/.Route( closure
	useNames []string // middleware .Use'd by every enclosing group closure
}

// exprName reduces a middleware expression to the name a human would call it:
// `auth.AuthMiddleware(keyStore, mgr)` → "AuthMiddleware".
func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.CallExpr:
		return exprName(v.Fun)
	}
	return ""
}

// usesDirectlyIn collects the callee name of every `x.Use(f...)` statement in
// this closure's own body. Nested groups get their own frame, so this must not
// recurse.
func usesDirectlyIn(body *ast.BlockStmt) []string {
	var out []string
	for _, stmt := range body.List {
		es, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Use" {
			continue
		}
		for _, arg := range call.Args {
			if n := exprName(arg); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// mountsFor parses main.go and reports, for each wanted route-path literal, the
// middleware chain of the chi group(s) its registration lexically sits in.
func mountsFor(t *testing.T, wanted map[string]bool) map[string]routeMount {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	out := map[string]routeMount{}

	// visit walks n carrying the accumulated middleware of the enclosing group
	// closures. isGroupArg says the FuncLit we are about to descend into was
	// passed to .Group(/.Route(.
	var visit func(n ast.Node, groupUses []string, depth int)
	visit = func(n ast.Node, groupUses []string, depth int) {
		if n == nil {
			return
		}
		switch v := n.(type) {
		case *ast.CallExpr:
			isGroupCall := false
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
				isGroupCall = sel.Sel.Name == "Group" || sel.Sel.Name == "Route"
			}
			// Record any wanted path literal appearing as an argument here.
			for _, arg := range v.Args {
				if bl, ok := arg.(*ast.BasicLit); ok && bl.Kind == token.STRING && wanted[bl.Value] {
					chain := append([]string(nil), groupUses...)
					out[bl.Value] = routeMount{
						line:     fset.Position(bl.Pos()).Line,
						inGroup:  depth > 0,
						useNames: chain,
					}
				}
			}
			for _, arg := range v.Args {
				if fl, ok := arg.(*ast.FuncLit); ok && isGroupCall {
					next := append(append([]string(nil), groupUses...), usesDirectlyIn(fl.Body)...)
					visit(fl.Body, next, depth+1)
					continue
				}
				visit(arg, groupUses, depth)
			}
			visit(v.Fun, groupUses, depth)
		default:
			for _, child := range childNodes(n) {
				visit(child, groupUses, depth)
			}
		}
	}
	visit(file, nil, 0)
	return out
}

// childNodes yields a node's direct children via ast.Inspect on the node itself,
// stopping at depth 1 so `visit` keeps control of the traversal.
func childNodes(n ast.Node) []ast.Node {
	var kids []ast.Node
	first := true
	ast.Inspect(n, func(c ast.Node) bool {
		if c == nil {
			return false
		}
		if first {
			first = false
			return true
		}
		kids = append(kids, c)
		return false
	})
	return kids
}

func TestPublicDiscoveryRoutesAreRegisteredWithoutAuth(t *testing.T) {
	wanted := map[string]bool{publicNodesPath: true, publicEmbedNodesPath: true}
	got := mountsFor(t, wanted)

	// Non-vacuity: both paths must be FOUND. A parse that finds nothing would
	// otherwise sail through every assertion below.
	for p := range wanted {
		if _, ok := got[p]; !ok {
			t.Fatalf("%s is not registered anywhere in main.go — either the route was removed "+
				"(then delete its projection in public_node_discovery.go) or this parse is "+
				"broken, and a broken parse passes everything", p)
		}
	}

	for p, m := range got {
		// It must genuinely be inside SOME group, otherwise "no auth middleware
		// here" is trivially true for the wrong reason.
		if !m.inGroup {
			t.Errorf("main.go:%d registers %s outside any chi group closure, so this guard is "+
				"reading an empty middleware chain and cannot tell an anonymous group from no "+
				"group at all", m.line, p)
			continue
		}
		if len(m.useNames) == 0 {
			t.Errorf("main.go:%d registers %s in a group that .Use's NOTHING — the anonymous "+
				"discovery group is rate-limited, and a chain that reads as empty means this "+
				"parse stopped seeing .Use calls", m.line, p)
		}
		for _, u := range m.useNames {
			if u == authMiddlewareName {
				t.Errorf("main.go:%d registers %s inside a group that .Use's %s.\n"+
					"    This route is no longer anonymous, so the narrowed public projection in "+
					"public_node_discovery.go (no url, no workspace_id) is now an unexplained "+
					"amputation of an AUTHENTICATED API rather than a leak fix. Decide which it "+
					"is — do not just delete this assertion.", m.line, p, u)
			}
		}
	}
}

// The other direction: nothing else in main.go may reach the cross-tenant
// available-node lister except through the projection. A third anonymous route
// that called it inline and wrote the rows straight out would reintroduce the
// leak with none of the tests above noticing.
func TestAvailableNodeListersAreReachedOnlyThroughTheProjection(t *testing.T) {
	src := readMainGo(t)

	// Non-vacuity: the two constructors must actually be wired here, or this
	// sweep is asserting the absence of something in a file that lost the feature.
	for _, ctor := range []string{
		"newPublicAvailableNodesHandler(",
		"newPublicAvailableEmbeddingNodesHandler(",
	} {
		if !strings.Contains(src, ctor) {
			t.Fatalf("main.go does not call %s — the projection is not wired, so 'nothing else "+
				"calls the lister' is true for the wrong reason", ctor)
		}
	}

	if n := strings.Count(src, "ListAvailableNodes("); n != 0 {
		t.Errorf("main.go calls ListAvailableNodes directly %d time(s). Both available-node routes "+
			"are anonymous and must go through newPublicAvailableNodesHandler / "+
			"newPublicAvailableEmbeddingNodesHandler, so the url/workspace_id projection applies.", n)
	}
}
