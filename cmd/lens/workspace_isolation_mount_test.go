package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// workspace_isolation_mount_test.go — guards the MOUNT SITE of the tenant-isolation middleware.
//
// workspaceIsolationMiddleware reads chi.URLParam(r, "wsID") and skips its check when that is
// empty. chi resolves URL params during route matching, which runs AFTER a top-level r.Use chain
// but BEFORE a Group's. Mounted top-level the parameter is therefore always "", the check skips,
// and every cross-tenant request is allowed — with no error, no log line, and no symptom.
//
// WHY THIS TEST EXISTS WHEN workspace_authz_test.go ALREADY PROBES THIS BEHAVIOURALLY.
// Its TestWorkspaceIsolationMiddleware_GroupPlacementResolvesParam says it "fails loudly" if a
// refactor moves the guard to a top-level Use. It does not, and cannot: it builds its own router
// and never reads main.go, so it is a canary for chi's SEMANTICS, not a guard on our CALL SITE.
// Measured, not assumed — with main.go's mount relocated to a bare top-level r.Use (tenant
// isolation silently disabled for every request):
//
//	existing behavioural tests ... ok      <- did not notice
//	this test .................... FAIL    <- named the file, line and consequence
//
// So the two are complements, not duplicates: the behavioural test proves the property and would
// catch a chi upgrade that changed when parameters resolve; this one proves the property is
// actually applied where it matters. Shape-checking is normally the weaker choice, but here the
// hazard IS a property of the call site, and no test that constructs its own router can observe
// it — not without run() being decomposed far beyond what this is worth.
func TestWorkspaceIsolationMiddleware_MountedWhereURLParamsResolve(t *testing.T) {
	const middlewareName = "workspaceIsolationMiddleware"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// Walk with a stack so each .Use call knows which closures enclose it. A mount qualifies when
	// it sits lexically inside a closure passed to .Group( or .Route( — the sub-router forms in
	// which chi has already matched the route, and so populated {wsID}, before middleware runs.
	type frame struct {
		node      ast.Node
		subRouter bool
		via       string
	}
	var stack []frame
	found := 0

	var walk func(n ast.Node) bool
	walk = func(n ast.Node) bool {
		if n == nil {
			return false
		}

		if fl, ok := n.(*ast.FuncLit); ok {
			sub, via := false, ""
			if len(stack) > 0 {
				if call, ok := stack[len(stack)-1].node.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						if sel.Sel.Name == "Group" || sel.Sel.Name == "Route" {
							sub, via = true, sel.Sel.Name
						}
					}
				}
			}
			stack = append(stack, frame{node: fl, subRouter: sub, via: via})
			ast.Inspect(fl.Body, walk)
			stack = stack[:len(stack)-1]
			return false
		}

		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Use" {
			// Not a .Use call: keep walking, but remember this call so a FuncLit argument can
			// see whether it was passed to Group/Route.
			stack = append(stack, frame{node: call})
			for _, a := range call.Args {
				ast.Inspect(a, walk)
			}
			stack = stack[:len(stack)-1]
			return false
		}

		mounts := false
		for _, a := range call.Args {
			if id, ok := a.(*ast.Ident); ok && id.Name == middlewareName {
				mounts = true
			}
		}
		if !mounts {
			return true
		}

		found++
		inSubRouter, via := false, ""
		for _, f := range stack {
			if f.subRouter {
				inSubRouter, via = true, f.via
			}
		}
		pos := fset.Position(call.Pos())
		if !inSubRouter {
			t.Errorf(
				"main.go:%d mounts %s with a bare top-level .Use — chi resolves {wsID} during route "+
					"matching, which runs AFTER a top-level middleware chain, so chi.URLParam(r, \"wsID\") "+
					"reads \"\" and the isolation check SKIPS EVERY REQUEST. It fails open and silently: no "+
					"error, no log, and every cross-tenant read succeeds. Mount it inside a .Group( or "+
					".Route( closure, as run() does.",
				pos.Line, middlewareName)
			return true
		}
		t.Logf("main.go:%d mounts %s inside a .%s( closure — parameters resolve", pos.Line, middlewareName, via)
		return true
	}
	ast.Inspect(file, walk)

	if found == 0 {
		t.Fatalf("no .Use(%s) call found in main.go — tenant isolation is not mounted at all, or this "+
			"guard has drifted from the code it protects", middlewareName)
	}
}
