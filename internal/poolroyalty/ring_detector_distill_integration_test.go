package poolroyalty

import (
	"context"
	"testing"
	"time"
)

// TestRingDetector_Distill_TransitiveRing proves the ring detector is genuinely
// table-parameterized: the SAME transitive-closure logic flags a self-dealing
// ring over distill_royalty_mints (the other cross-tenant reuse faucet), which
// shares the contributor/requester/status/created_at columns the detector reads.
func TestRingDetector_Distill_TransitiveRing(t *testing.T) {
	pool, _ := distillMintHarness(t)
	ctx := context.Background()
	// The edge tables distillMintHarness doesn't create.
	linkExec(t, pool, `CREATE TABLE IF NOT EXISTS workspace_card_fingerprints (
		workspace_id TEXT NOT NULL, fingerprint_hash TEXT NOT NULL, PRIMARY KEY (workspace_id, fingerprint_hash))`)
	linkExec(t, pool, `CREATE TABLE IF NOT EXISTS workspace_owner_links (
		workspace_id TEXT NOT NULL, owner_key TEXT NOT NULL, PRIMARY KEY (workspace_id, owner_key))`)
	linkExec(t, pool, `TRUNCATE workspace_card_fingerprints, workspace_owner_links, distill_royalty_mints`)

	seedHeld := func(reqID, contributor, requester string) {
		linkExec(t, pool, `INSERT INTO distill_royalty_mints
			(request_id, contributor_workspace_id, requester_workspace_id, content_hash, avoided_cogs_usd, minted_amount, status, finalize_after)
			VALUES ($1,$2,$3,$4,2.0,1000000,'held', now()+interval '1 hour')`,
			reqID, contributor, requester, "c-"+reqID)
	}
	// Ring among one operator's A,B,C; honest reuse P→Q.
	seedHeld("d-ab", "dwA", "dwB")
	seedHeld("d-bc", "dwB", "dwC")
	seedHeld("d-ca", "dwC", "dwA") // transitive pair
	seedHeld("d-hon", "dwP", "dwQ")
	// Transitive linkage: A-B via k1, B-C via k2 (A,C not directly linked).
	linkExec(t, pool, `INSERT INTO workspace_owner_links (workspace_id, owner_key) VALUES
		('dwA','dk1'), ('dwB','dk1'), ('dwB','dk2'), ('dwC','dk2')`)

	flags, err := NewRingDetector(pool, "distill_royalty_mints").DetectSelfDealingRings(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("DetectSelfDealingRings(distill): %v", err)
	}
	got := map[string]bool{}
	for _, f := range flags {
		got[f.RequestID] = true
	}
	for _, id := range []string{"d-ab", "d-bc", "d-ca"} {
		if !got[id] {
			t.Errorf("distill ring mint %s must be flagged", id)
		}
	}
	if got["d-hon"] {
		t.Error("honest distill reuse must NOT be flagged")
	}
}
