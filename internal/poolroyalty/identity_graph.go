// identity_graph.go — Phase-2 anti-gaming: the transitive-closure identity graph.
//
// Every existing self-deal check in the codebase (sharedFingerprintSQL,
// ownerLinkedSQL) is PAIRWISE — it asks "are these two workspaces directly
// linked?" A self-dealing RING evades that: link A↔B and B↔C but never A↔C, and
// the mint (contributor C, requester A) passes every pairwise test while all
// three workspaces are one operator. This graph closes the hole with union-find:
// two workspaces are the same operator iff they land in the same connected
// component of the identity edges (card fingerprint ∪ owner_key), transitively.
//
// Pure in-memory structure — no DB, no lock, no money. LoadIdentityGraph
// (identity_graph_load.go) populates it from the two edge tables; this file is
// just the disjoint-set logic so the ring reasoning is unit-testable without PG.
package poolroyalty

// IdentityGraph is a union-find (disjoint-set) over workspace ids. Same component
// ⇒ same operator (transitively). The zero value is unusable — use
// NewIdentityGraph.
type IdentityGraph struct {
	parent map[string]string
	rank   map[string]int
}

// NewIdentityGraph returns an empty graph. Unknown workspaces are their own
// singleton component (so two never-linked workspaces are different operators).
func NewIdentityGraph() *IdentityGraph {
	return &IdentityGraph{parent: map[string]string{}, rank: map[string]int{}}
}

// ensure lazily makes x its own component the first time it is seen.
func (g *IdentityGraph) ensure(x string) {
	if _, ok := g.parent[x]; !ok {
		g.parent[x] = x
		g.rank[x] = 0
	}
}

// find returns x's component representative with path compression.
func (g *IdentityGraph) find(x string) string {
	g.ensure(x)
	for g.parent[x] != x {
		g.parent[x] = g.parent[g.parent[x]] // path halving
		x = g.parent[x]
	}
	return x
}

// Link unions the components of a and b (an identity edge: shared fingerprint or
// shared owner_key). Idempotent and order-independent.
func (g *IdentityGraph) Link(a, b string) {
	ra, rb := g.find(a), g.find(b)
	if ra == rb {
		return
	}
	// Union by rank keeps the tree shallow.
	switch {
	case g.rank[ra] < g.rank[rb]:
		g.parent[ra] = rb
	case g.rank[ra] > g.rank[rb]:
		g.parent[rb] = ra
	default:
		g.parent[rb] = ra
		g.rank[ra]++
	}
}

// SameOperator reports whether a and b are in one connected component — the
// transitive same-operator test that a pairwise edge check misses. Reflexive
// (a==b ⇒ true), symmetric, and transitive by construction.
func (g *IdentityGraph) SameOperator(a, b string) bool {
	if a == b {
		return true
	}
	return g.find(a) == g.find(b)
}

// Component returns a stable representative id for x's component — for evidence
// (which cluster a flag belongs to) and grouping. Two workspaces share a
// Component value iff SameOperator is true.
func (g *IdentityGraph) Component(x string) string { return g.find(x) }
