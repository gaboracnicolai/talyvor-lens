package mining

import (
	"context"
	"testing"
	"time"
)

// Phase-3 Item 2 — traffic_mint_holds clawback. The generic poolroyalty.Revoker
// is column-incompatible with this table (it RETURNs contributor_workspace_id and
// keys on request_id alone, but traffic_mint_holds has workspace_id and a
// composite PK (request_id, workspace_id, mint_type)). So the cache/compute/
// embedding node mints had NO reachable revoker. TrafficRevoker closes that.

// seedTrafficHeld mints a HELD traffic row (held_balance credited + a
// traffic_mint_holds claim) via the real CreditOnceHeld path.
func seedTrafficHeld(t *testing.T, ledger *LedgerStore, reqID, ws, mintType string, amount int64) {
	t.Helper()
	if _, err := ledger.CreditOnceHeld(context.Background(), reqID, ws, amount, mintType, "seed traffic held", time.Hour, nil); err != nil {
		t.Fatalf("seed CreditOnceHeld: %v", err)
	}
}

// RED (Phase-3 Item 2): a held traffic mint (cache/compute/embedding) must be
// revocable before finalize — status held→revoked and the held balance burned.
// Before TrafficRevoker there was no revoker that could reach traffic_mint_holds
// (the composite key + workspace_id column), so the holdback was decorative for
// the node mints. This proves the clawback.
func TestTrafficRevoker_RevokesHeldBeforeFinalize_Integration(t *testing.T) {
	pool := trafficHeldHarness(t, "node-owner")
	ctx := context.Background()
	ledger := NewLedgerStore(pool)
	seedTrafficHeld(t, ledger, "treq-1", "node-owner", TypeComputeMine, 50_000)
	if held := heldBalance(t, pool, "node-owner"); held != 50_000 {
		t.Fatalf("pre-revoke held=%d, want 50000", held)
	}

	rev := NewTrafficRevoker(pool, ledger)
	rep, err := rev.RevokeTrafficHolds(ctx, []TrafficHoldKey{{RequestID: "treq-1", WorkspaceID: "node-owner", MintType: TypeComputeMine}})
	if err != nil {
		t.Fatalf("RevokeTrafficHolds: %v", err)
	}
	key := "treq-1|node-owner|" + TypeComputeMine
	if rep.Outcomes[key] != TrafficRevokeRevoked {
		t.Fatalf("outcome %v, want revoked (a held traffic mint must be reversible before finalize)", rep.Outcomes[key])
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM traffic_mint_holds WHERE request_id='treq-1' AND workspace_id='node-owner' AND mint_type=$1`, TypeComputeMine).Scan(&status)
	if status != "revoked" {
		t.Fatalf("status=%q, want revoked", status)
	}
	if held := heldBalance(t, pool, "node-owner"); held != 0 {
		t.Fatalf("post-revoke held=%d, want 0 (burned)", held)
	}
	// Spendable never moved — a revoked held mint never enters circulation.
	if sp := spendableBalance(t, pool, "node-owner"); sp != 0 {
		t.Fatalf("post-revoke spendable=%d, want 0", sp)
	}
	if supply, _ := ledger.GetTotalSupply(ctx); supply != 0 {
		t.Fatalf("post-revoke supply=%d, want 0 (a revoked held mint never counted)", supply)
	}
}

// A finalized traffic mint can NEVER be revoked (the CAS matches status='held'
// only), and a revoke is exactly-once + idempotent.
func TestTrafficRevoker_FinalizedNotRevocable_And_Idempotent_Integration(t *testing.T) {
	pool := trafficHeldHarness(t, "node-owner")
	ctx := context.Background()
	ledger := NewLedgerStore(pool)
	rev := NewTrafficRevoker(pool, ledger)

	// finalized row → not revocable (isolated workspace "wf").
	seedTrafficHeld(t, ledger, "treq-2", "wf", TypeCacheMine, 10_000)
	if _, err := pool.Exec(ctx, `UPDATE traffic_mint_holds SET status='final' WHERE request_id='treq-2'`); err != nil {
		t.Fatal(err)
	}
	rep, _ := rev.RevokeTrafficHolds(ctx, []TrafficHoldKey{{RequestID: "treq-2", WorkspaceID: "wf", MintType: TypeCacheMine}})
	if rep.Outcomes["treq-2|wf|"+TypeCacheMine] != TrafficRevokeSkippedNotHeld {
		t.Fatalf("finalized mint outcome %v, want skipped_not_held (a final row is never revocable)", rep.Outcomes["treq-2|wf|"+TypeCacheMine])
	}

	// idempotent: revoke a held row twice → second is skipped_already_revoked, no
	// double-burn (isolated workspace "wi").
	seedTrafficHeld(t, ledger, "treq-3", "wi", TypeCacheMine, 10_000)
	k3 := TrafficHoldKey{RequestID: "treq-3", WorkspaceID: "wi", MintType: TypeCacheMine}
	_, _ = rev.RevokeTrafficHolds(ctx, []TrafficHoldKey{k3})
	if held := heldBalance(t, pool, "wi"); held != 0 {
		t.Fatalf("after first revoke held=%d, want 0", held)
	}
	rep2, _ := rev.RevokeTrafficHolds(ctx, []TrafficHoldKey{k3})
	if rep2.Outcomes["treq-3|wi|"+TypeCacheMine] != TrafficRevokeSkippedAlreadyRevoked {
		t.Fatalf("second revoke outcome %v, want skipped_already_revoked (no double-burn)", rep2.Outcomes["treq-3|wi|"+TypeCacheMine])
	}
	if held := heldBalance(t, pool, "wi"); held != 0 {
		t.Fatalf("after idempotent second revoke held=%d, want 0 (no double-burn/negative)", held)
	}
}
