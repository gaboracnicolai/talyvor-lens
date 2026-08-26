package main

import (
	"go/ast"
	"go/token"
	"testing"
)

// earnings_route_wiring_test.go — the wiring half of W4.6.1 step 7's read route, asserted on
// main.go itself, for the reason economy_wiring_test.go gives one directory over: a money feature
// fails in two independent ways, the arithmetic being wrong and NOTHING CALLING IT, and a
// behavioural test that builds its own router is blind to the second.
//
// The specific hazard here is narrower than "the route is missing". `earning_enabled` is the field
// that lets a reader tell "this workspace earned nothing" from "earning is switched off in this
// deployment". If its inputs were ever hardcoded — the literal `true` typed while debugging is the
// classic — the route would answer confidently and wrongly, and no behavioural test that supplies
// its own Gates could see it, because supplying Gates is exactly what those tests do.

func TestEarningsRoute_IsRegisteredInTheIsolatedGroup(t *testing.T) {
	_, f := mainGoAST(t)

	const pattern = "/v1/workspaces/{wsID}/earnings"
	var found bool
	var receiver string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || lit.Value != `"`+pattern+`"` {
			return true
		}
		found = true
		// econ.get(authed, "…", h) — arg 0 is the router the route is hung on.
		if id, ok := call.Args[0].(*ast.Ident); ok {
			receiver = id.Name
		}
		return true
	})

	if !found {
		t.Fatalf("[EW1] main.go registers no route at %s. The reader can be perfect and the surface "+
			"still does not exist.", pattern)
	}
	// It must hang off the SAME router group that carries workspaceIsolationMiddleware. A
	// workspace-scoped money read mounted anywhere else is a cross-tenant read.
	if receiver != "authed" {
		t.Fatalf("[EW2] %s is registered on %q, not on the `authed` group. Only that group applies "+
			"workspaceIsolationMiddleware (main.go's authed.Use), so anywhere else this route serves "+
			"any workspace's earnings to any authenticated caller.", pattern, receiver)
	}
}

func TestEarningsRoute_GatesComeFromConfigAndNotFromLiterals(t *testing.T) {
	_, f := mainGoAST(t)

	var lit *ast.CompositeLit
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Gates" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "earnings" {
			lit = cl
		}
		return true
	})
	if lit == nil {
		t.Fatalf("[EW3] main.go builds no earnings.Gates literal — the route cannot be reporting the " +
			"deployment's real switches, so `earning_enabled` means nothing")
	}

	// FLOOR: every field of Gates must be supplied. A field left at its zero value reads as "off"
	// and would put a gate in disabled_gates that the operator has actually turned on.
	want := map[string]bool{
		"EconomyEnabled": false, "PoolRoyaltyMintingEnabled": false,
		"CachePoolableEnabled": false, "DistillPoolableEnabled": false,
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		if _, known := want[key.Name]; !known {
			t.Errorf("[EW4-UNKNOWN] earnings.Gates has a field %q this guard does not know about — "+
				"add it here WITH the config value it must come from, or the new switch is unguarded", key.Name)
			continue
		}
		want[key.Name] = true

		// The value must be a cfg.<Something> selector. A literal here is the whole hazard.
		sel, ok := kv.Value.(*ast.SelectorExpr)
		if !ok {
			t.Errorf("[EW5] earnings.Gates.%s is not a config selector — a hardcoded value makes "+
				"earning_enabled a claim about the source rather than about the deployment", key.Name)
			continue
		}
		base, ok := sel.X.(*ast.Ident)
		if !ok || base.Name != "cfg" {
			t.Errorf("[EW5-SRC] earnings.Gates.%s reads from %v, not from cfg", key.Name, sel.X)
		}
	}
	for name, supplied := range want {
		if !supplied {
			t.Errorf("[EW6] earnings.Gates.%s is not set at the call site, so it defaults to false and "+
				"the route reports that switch as disabled whatever the operator configured", name)
		}
	}
}
