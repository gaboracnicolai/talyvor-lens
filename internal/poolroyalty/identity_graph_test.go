package poolroyalty

import "testing"

// TestIdentityGraph_TransitiveRing is the RING insight in one test: a pairwise
// (direct-edge) check misses A↔C when the operator only linked A↔B and B↔C, but
// connected-components (transitive closure) puts A, B, C in one operator. This is
// exactly the evasion the spec calls out — "A→B→C→A is the obvious evasion of a
// pairwise check."
func TestIdentityGraph_TransitiveRing(t *testing.T) {
	g := NewIdentityGraph()
	g.Link("A", "B") // e.g. shared owner_key k1
	g.Link("B", "C") // e.g. shared owner_key k2 — A and C are NOT directly linked

	// The whole point: A and C are the same operator via B, though no direct edge.
	if !g.SameOperator("A", "C") {
		t.Fatal("A and C must be same-operator via transitive closure (B) — a pairwise check would MISS this ring")
	}
	if !g.SameOperator("A", "B") || !g.SameOperator("B", "C") {
		t.Fatal("directly-linked pairs must be same-operator")
	}
	// Symmetry + reflexivity.
	if !g.SameOperator("C", "A") || !g.SameOperator("A", "A") {
		t.Fatal("same-operator must be symmetric and reflexive")
	}
	// One shared component id across the ring.
	if g.Component("A") != g.Component("C") {
		t.Fatal("A and C must share a component id")
	}
}

// TestIdentityGraph_DistinctOperators — unlinked workspaces are different
// operators (no false positive on honest cross-tenant reuse).
func TestIdentityGraph_DistinctOperators(t *testing.T) {
	g := NewIdentityGraph()
	g.Link("A", "B")
	g.Link("X", "Y") // a separate operator

	if g.SameOperator("A", "X") {
		t.Fatal("A (operator 1) and X (operator 2) must NOT be same-operator")
	}
	if g.SameOperator("A", "Z") {
		t.Fatal("an unseen workspace Z shares no operator with A")
	}
	if g.SameOperator("A", "A") != true {
		t.Fatal("reflexivity: a workspace is its own operator")
	}
	// Two never-linked workspaces are not the same operator even if both unseen.
	if g.SameOperator("P", "Q") {
		t.Fatal("two distinct unseen workspaces are not the same operator")
	}
}
