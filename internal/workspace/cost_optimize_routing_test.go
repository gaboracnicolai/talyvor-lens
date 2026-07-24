package workspace

import (
	"context"
	"testing"
)

// Default FALSE — a named model is honoured, cost-optimised routing is off until
// the workspace consents. Unknown workspace also FALSE (fail-safe).
func TestCostOptimizeRouting_DefaultFalse(t *testing.T) {
	m := New(nil)
	if m.GetCostOptimizeRouting("unknown-ws") {
		t.Error("unknown workspace must default to FALSE (honour named model)")
	}
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})
	if m.GetCostOptimizeRouting("w") {
		t.Error("a registered workspace with no opt-in must default to FALSE")
	}
}

// Set flips it in memory and is readable on the next call; unregistered errors.
func TestSetCostOptimizeRouting(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})
	if err := m.SetCostOptimizeRouting(context.Background(), "w", true); err != nil {
		t.Fatal(err)
	}
	if !m.GetCostOptimizeRouting("w") {
		t.Error("after opt-in, GetCostOptimizeRouting must be true")
	}
	if err := m.SetCostOptimizeRouting(context.Background(), "nope", true); err == nil {
		t.Error("setting an unregistered workspace should error")
	}
}

// RegisterWorkspace preserves an explicit opt-in through the register upsert.
func TestRegisterWorkspace_PreservesCostOptimizeRouting(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true, CostOptimizeRouting: true})
	if !m.GetCostOptimizeRouting("w") {
		t.Error("an explicit CostOptimizeRouting=true must be preserved on register")
	}
}
