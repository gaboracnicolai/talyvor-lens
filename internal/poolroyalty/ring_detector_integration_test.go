package poolroyalty

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/mining"
)

// seedHeldFor mints one HELD pool-royalty row with an EXPLICIT (contributor,
// requester) using a linkage-OFF minter, so the ring's held rows exist
// regardless of the mint-time guard (modeling transitive / late-declared
// linkage the pairwise mint-time check could not see). Returns the request_id.
func seedHeldFor(t *testing.T, m *Minter, ctx context.Context, reqID, contributor, requester string) string {
	t.Helper()
	h := ServedHit{
		RequestID: reqID, RequesterWorkspace: requester, ContributorWorkspace: contributor,
		Layer: "exact", EntryID: "e-" + reqID, Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0, // s=0.5 ⇒ minted 1.0 LENS held
		AnswerSHA256:   SHA256Hex([]byte("a:" + reqID + ":" + contributor + ":" + requester)),
		PromptSHA256:   SHA256Hex([]byte("p:" + reqID)),
	}
	res, err := m.MintServedHit(ctx, h)
	if err != nil || !res.Minted {
		t.Fatalf("seed held mint %s: res=%+v err=%v", reqID, res, err)
	}
	return res.RequestID
}

// TestRingDetector_TransitiveRing_Flagged_HonestNot is the Phase-2 detector
// proof on real PG:
//
//   - A self-dealing RING of 3 workspaces (one operator) with TRANSITIVE linkage
//     (A↔B via owner_key k1, B↔C via k2, A and C NOT directly linked) → every
//     held ring mint is flagged, INCLUDING the (contributor C, requester A) mint
//     that a pairwise direct-edge check would MISS. This is the ring insight.
//   - Honest cross-tenant reuse (P, Q — different operators, no shared key) → NOT
//     flagged. No false positive.
func TestRingDetector_TransitiveRing_Flagged_HonestNot(t *testing.T) {
	pool := linkageTestPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)
	// linkage OFF: the ring's held rows must exist for the detector to adjudicate.
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Hour) // stays held; no finalize during the test

	// The ring: circular reuse among A, B, C.
	ringAB := seedHeldFor(t, m, ctx, "ring-ab", "wsA", "wsB") // B reused A
	ringBC := seedHeldFor(t, m, ctx, "ring-bc", "wsB", "wsC") // C reused B
	ringCA := seedHeldFor(t, m, ctx, "ring-ca", "wsC", "wsA") // A reused C — the transitive pair

	// Honest cross-tenant reuse: different operators, no shared identity.
	honest := seedHeldFor(t, m, ctx, "honest-pq", "wsP", "wsQ")

	// TRANSITIVE linkage: A-B via k1, B-C via k2. A and C share NO direct key.
	linkExec(t, pool, `INSERT INTO workspace_owner_links (workspace_id, owner_key) VALUES
		('wsA','op-k1'), ('wsB','op-k1'),
		('wsB','op-k2'), ('wsC','op-k2')`)
	// (wsP, wsQ deliberately have no edges.)

	d := NewRingDetector(pool, "pool_royalty_mints")
	flags, err := d.DetectSelfDealingRings(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("DetectSelfDealingRings: %v", err)
	}

	flagged := map[string]RingFlag{}
	for _, f := range flags {
		flagged[f.RequestID] = f
	}

	// All three ring mints flagged — including the transitive (C→A) one.
	for _, id := range []string{ringAB, ringBC, ringCA} {
		if _, ok := flagged[id]; !ok {
			t.Errorf("ring mint %s must be flagged (all three workspaces are one operator)", id)
		}
	}
	if _, ok := flagged[ringCA]; !ok {
		t.Error("the (contributor C, requester A) mint MUST be flagged via transitive closure — a pairwise check would MISS it (this is the whole point)")
	}
	// Evidence is explainable: same component id, a human-readable reason.
	if f, ok := flagged[ringCA]; ok {
		if f.ComponentID == "" || f.Reason == "" {
			t.Error("a flag must carry evidence: component id + reason")
		}
	}
	// The honest reuse is NOT flagged.
	if _, ok := flagged[honest]; ok {
		t.Error("honest cross-tenant reuse (different operators) must NOT be flagged — false positive")
	}
}
