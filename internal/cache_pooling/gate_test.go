package cache_pooling

import (
	"context"
	"testing"
)

// gate builds a PoolabilityGate over a fixed global switch and a set of
// poolable workspaces.
func gate(global bool, poolable ...string) *PoolabilityGate {
	set := map[string]bool{}
	for _, w := range poolable {
		set[w] = true
	}
	return New(func() bool { return global }, func(ws string) bool { return set[ws] })
}

// A nil gate (pooling not wired) is always-false and never panics — fully inert.
func TestGate_NilIsInert(t *testing.T) {
	var g *PoolabilityGate
	if g.Participant("a") || g.DecidePoolableOnWrite(context.Background(), "a") || g.MaybeAllowPooledHit(context.Background(), "a", "b") {
		t.Error("a nil gate must report false for every decision")
	}
}

// Global OFF blocks everything regardless of per-workspace opt-in.
func TestGate_GlobalOffBlocksAll(t *testing.T) {
	g := gate(false, "a", "b")
	if g.Participant("a") {
		t.Error("global off → not a participant")
	}
	if g.DecidePoolableOnWrite(context.Background(), "a") {
		t.Error("global off → no pooled write")
	}
	if g.MaybeAllowPooledHit(context.Background(), "a", "b") {
		t.Error("global off → no pooled hit even with both workspaces opted in")
	}
}

// A pooled hit requires ALL THREE: global on, requester opted in, contributor
// opted in. Each missing one blocks.
func TestGate_PooledHitRequiresAllThree(t *testing.T) {
	// All three present → allowed.
	if !gate(true, "req", "owner").MaybeAllowPooledHit(context.Background(), "req", "owner") {
		t.Error("global on + requester opted in + contributor opted in → pooled hit allowed")
	}
	// Requester NOT opted in → blocked.
	if gate(true, "owner").MaybeAllowPooledHit(context.Background(), "req", "owner") {
		t.Error("requester not opted in → blocked")
	}
	// Contributor (owner) NOT opted in → blocked.
	if gate(true, "req").MaybeAllowPooledHit(context.Background(), "req", "owner") {
		t.Error("contributor not opted in → blocked")
	}
	// Global off → blocked (covered above, asserted here for the matrix).
	if gate(false, "req", "owner").MaybeAllowPooledHit(context.Background(), "req", "owner") {
		t.Error("global off → blocked")
	}
}

// A pooled entry with NO recorded owner (a pre-feature entry) is never poolable
// — backward-compat safety.
func TestGate_EmptyOwnerNeverPoolable(t *testing.T) {
	if gate(true, "req").MaybeAllowPooledHit(context.Background(), "req", "") {
		t.Error("an entry with no contributor provenance must never be served cross-tenant")
	}
}

// DecidePoolableOnWrite mirrors participant status for the contributor.
func TestGate_DecidePoolableOnWrite(t *testing.T) {
	if !gate(true, "c").DecidePoolableOnWrite(context.Background(), "c") {
		t.Error("global on + contributor opted in → write to pool")
	}
	if gate(true).DecidePoolableOnWrite(context.Background(), "c") {
		t.Error("contributor not opted in → no pooled write")
	}
	if gate(false, "c").DecidePoolableOnWrite(context.Background(), "c") {
		t.Error("global off → no pooled write")
	}
}
