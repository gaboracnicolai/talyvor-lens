package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// THE ECONOMY KNOBS ARE WIRED — asserted on main.go itself.
//
// ⚠ WHY A WIRING TEST AND NOT ONLY A BEHAVIOURAL ONE. A money feature fails in two independent
// ways: the arithmetic is wrong, or nothing calls it. Behavioural tests construct their own
// Proxy and set what they need, so they prove the arithmetic and are BLIND to the second. That
// is not hypothetical here — the pooled-consumer discount shipped, was reverted wholesale by an
// unrelated PR merged from a stale base, and every test stayed green because the tests were
// deleted with the code. Live, consumers paid list price on cross-tenant pooled hits for days.
//
// This reads the actual wiring in run(). It cannot be satisfied by a test's own setup, and a
// revert that removes the call fails it even if it also removes every other test.
//
// It deliberately checks the CALL, not the config field: a config value nothing passes to the
// proxy is exactly as inert as no config at all, and that is the failure mode this catches.

func mainGoAST(t *testing.T) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	return fset, f
}

// calledWith reports whether main.go contains a call to method `name` whose argument list
// mentions `argSubstr` (so the test pins that the CONFIG value is what gets passed, not a
// literal someone hardcoded while debugging).
func calledWith(t *testing.T, f *ast.File, name, argSubstr string) bool {
	t.Helper()
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if argSubstr == "" {
			found = true
			return false
		}
		for _, a := range call.Args {
			if strings.Contains(exprText(a), argSubstr) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// exprText renders an expression well enough to look for an identifier in it.
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		parts := []string{exprText(v.Fun)}
		for _, a := range v.Args {
			parts = append(parts, exprText(a))
		}
		return strings.Join(parts, " ")
	case *ast.BinaryExpr:
		// ⚠ ADDED AFTER THIS FILE'S OWN GUARD SILENTLY MATCHED NOTHING. Without this case a
		// condition like `a && !b` rendered as the empty string, so every Contains() check on it
		// was false and the test failed for a reason that had nothing to do with main.go.
		return exprText(v.X) + " " + v.Op.String() + " " + exprText(v.Y)
	case *ast.UnaryExpr:
		return v.Op.String() + exprText(v.X)
	case *ast.ParenExpr:
		return exprText(v.X)
	case *ast.FuncLit:
		var out []string
		ast.Inspect(v, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				out = append(out, id.Name)
			}
			return true
		})
		return strings.Join(out, ".")
	}
	return ""
}

// TestPoolConsumerDiscountIsWiredFromConfig — the discount reaches the proxy.
//
// Without this call the feature is present, tested, and dead: every pooled hit charges list
// price and the ledger row records a rate of 0. That is precisely what the deployment did.
func TestPoolConsumerDiscountIsWiredFromConfig(t *testing.T) {
	_, f := mainGoAST(t)
	if !calledWith(t, f, "SetPoolConsumerDiscount", "PoolConsumerDiscount") {
		t.Error("main.go does not call SetPoolConsumerDiscount(cfg.PoolConsumerDiscount).\n" +
			"The rate then stays 0 and every cross-tenant pooled hit charges the consumer LIST " +
			"PRICE, with pool_discount_rate=0 on the ledger row. The code being present and " +
			"tested does not make it run.")
	}
}

// TestPoolConsumerDiscountIsNotGatedOnMinting pins a decision that is easy to "tidy" away.
//
// The discount is wired unconditionally and NOT beside the royalty minter, because a pooled hit
// CHARGES the consumer whether or not royalty minting is enabled. Gating the discount on the
// mint flag would leave consumers paying full price on any deploy with minting off — which is
// every deploy today, since LENS_POOL_ROYALTY_MINTING_ENABLED defaults false.
func TestPoolConsumerDiscountIsNotGatedOnMinting(t *testing.T) {
	fset, f := mainGoAST(t)
	var offending string
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		cond := exprText(ifs.Cond)
		if !strings.Contains(cond, "PoolRoyaltyMintingEnabled") && !strings.Contains(cond, "MintingEnabled") {
			return true
		}
		ast.Inspect(ifs.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetPoolConsumerDiscount" {
				offending = fset.Position(call.Pos()).String()
			}
			return true
		})
		return true
	})
	if offending != "" {
		t.Errorf("SetPoolConsumerDiscount is inside a minting-enabled branch at %s.\n"+
			"The consumer is CHARGED for a pooled hit whether or not the royalty mints, so "+
			"gating the discount on the mint flag reintroduces full-price pooled hits on every "+
			"deploy with minting off — through the wiring rather than the code.", offending)
	}
}

// TestRoyaltyMinterIsWired — the other half of the same seam.
//
// The mint being OFF is a deliberate default; the mint being UNWIRED would be silent in exactly
// the same way, and indistinguishable from outside. Assert the call exists so the flag remains
// the only thing deciding.
func TestRoyaltyMinterIsWired(t *testing.T) {
	_, f := mainGoAST(t)
	if !calledWith(t, f, "SetRoyaltyMinter", "") {
		t.Error("main.go does not call SetRoyaltyMinter — pooled hits would mint nothing, and " +
			"mintPooledRoyalty returns silently on a nil minter, so nothing would say why.")
	}
}

// TestPooledWithoutMintingIsAnnounced — the silence that cost a full investigation.
//
// An operator verified all THREE pooling gates (the global flag, the pool-safety attestation, and
// both workspaces' cache_poolable + earn_verified), served many pooled hits, and found
// lens_token_ledger empty with nothing in the log to explain it. The reason is a FOURTH gate those
// three say nothing about — LENS_POOL_ROYALTY_MINTING_ENABLED, default false — and its off state
// is completely silent: Minter.MintServedHit returns (Result{}, nil) before touching the database,
// and mintPooledRoyalty only logs on an ERROR.
//
// A default that is correct can still be a defect if nothing can tell you it is in force.
func TestPooledWithoutMintingIsAnnounced(t *testing.T) {
	_, f := mainGoAST(t)
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		cond := exprText(ifs.Cond)
		if !strings.Contains(cond, "CachePoolableEnabled") ||
			!strings.Contains(cond, "PoolRoyaltyMintingEnabled") {
			return true
		}
		ast.Inspect(ifs.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok &&
				(sel.Sel.Name == "Warn" || sel.Sel.Name == "Error") {
				found = true
			}
			return true
		})
		return true
	})
	if !found {
		t.Error("boot does not announce the pooling-on / minting-off combination.\n" +
			"In that state pooled hits serve and charge, contributors earn nothing, and NOTHING " +
			"says so: no row in pool_royalty_mints, none in lens_token_ledger, no log line. The " +
			"operator is left checking gates that are all open.")
	}
}
