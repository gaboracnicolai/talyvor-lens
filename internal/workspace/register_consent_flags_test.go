package workspace

import (
	"context"
	"testing"
)

// register_consent_flags_test.go — a blind re-registration must not move ANY cross-tenant consent
// flag, in EITHER direction.
//
// #357 closed this for cache_poolable by removing it from the ON CONFLICT clause. distill_poolable
// and cost_optimize_routing were left in that clause, so both still moved on a re-POST:
//
//	field ABSENT  -> the Go zero value (false) is written  => SILENT REVOCATION of an opt-in
//	explicit TRUE -> the body value is written             => RETROACTIVE GRANT on an opted-out row
//
// Both were proven by running the code, not by reading it. distill_poolable is the more sensitive of
// the two: it governs DOCUMENT-DERIVED artifacts ("a more sensitive disclosure than a prompt/response
// pair", per distill_poolable.go), and revocation additionally breaks the owner's distill royalties.
//
// The rule these tests pin: registration CREATES consent, it never CHANGES it. The dedicated setters
// (SetDistillPoolable / SetCostOptimizeRouting) and their per-workspace routes remain the only way to
// change an existing workspace — which is already what docs/staging-economy-turnon.md instructs.

func storedFlags(t *testing.T, m *Manager, wsID string) (distill, costOpt bool) {
	t.Helper()
	if err := m.pool.QueryRow(context.Background(),
		`SELECT distill_poolable, cost_optimize_routing FROM workspaces WHERE id=$1`, wsID).
		Scan(&distill, &costOpt); err != nil {
		t.Fatalf("read stored flags for %q: %v", wsID, err)
	}
	return
}

// ⚠ SILENT REVOCATION. A workspace that opted IN to cross-tenant distill sharing must not be opted
// out by a re-registration that simply says nothing about the flag.
func TestRegisterWorkspace_ReRegistration_DoesNotRevokeDistillPoolable(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	m := New(pool)

	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-d-in", Name: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetDistillPoolable(ctx, "ws-d-in", true); err != nil {
		t.Fatal(err)
	}

	// A blind re-POST with the field ABSENT.
	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-d-in", Name: "d2"}); err != nil {
		t.Fatal(err)
	}

	distill, _ := storedFlags(t, m, "ws-d-in")
	if !distill {
		t.Error("stored distill_poolable=false: a re-registration with the field absent SILENTLY REVOKED an opt-in (and the owner's distill royalties with it)")
	}
	if !m.GetDistillPoolable("ws-d-in") {
		t.Error("in-memory distill_poolable was left revoked — RETURNING must reconcile it to the persisted value")
	}
}

// ⚠ RETROACTIVE GRANT. An opted-OUT workspace must not be opted in by a re-registration carrying an
// explicit true. Consent to share document-derived artifacts cannot be granted on the tenant's behalf.
func TestRegisterWorkspace_ReRegistration_DoesNotGrantDistillPoolable(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	m := New(pool)

	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-d-out", Name: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetDistillPoolable(ctx, "ws-d-out", false); err != nil {
		t.Fatal(err)
	}

	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-d-out", Name: "d2", DistillPoolable: true}); err != nil {
		t.Fatal(err)
	}

	distill, _ := storedFlags(t, m, "ws-d-out")
	if distill {
		t.Error("stored distill_poolable=true: a re-registration RETROACTIVELY GRANTED consent to share document-derived artifacts")
	}
	if m.GetDistillPoolable("ws-d-out") {
		t.Error("in-memory distill_poolable was granted — RETURNING must reconcile it to the persisted false")
	}
}

// Same two directions for cost_optimize_routing — #349's consent flag (permission to serve a cheaper
// model than the one asked for). An absent field must not revoke it...
func TestRegisterWorkspace_ReRegistration_DoesNotRevokeCostOptimizeRouting(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	m := New(pool)

	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-c-in", Name: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetCostOptimizeRouting(ctx, "ws-c-in", true); err != nil {
		t.Fatal(err)
	}

	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-c-in", Name: "c2"}); err != nil {
		t.Fatal(err)
	}

	_, costOpt := storedFlags(t, m, "ws-c-in")
	if !costOpt {
		t.Error("stored cost_optimize_routing=false: a re-registration with the field absent silently revoked routing consent")
	}
	if !m.GetCostOptimizeRouting("ws-c-in") {
		t.Error("in-memory cost_optimize_routing was left revoked — RETURNING must reconcile it")
	}
}

// ...and an explicit true must not grant it. Routing consent is the permission not to get the model
// you named; granting it by a blind upsert is the #349 violation re-introduced through the back door.
func TestRegisterWorkspace_ReRegistration_DoesNotGrantCostOptimizeRouting(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	m := New(pool)

	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-c-out", Name: "c"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetCostOptimizeRouting(ctx, "ws-c-out", false); err != nil {
		t.Fatal(err)
	}

	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-c-out", Name: "c2", CostOptimizeRouting: true}); err != nil {
		t.Fatal(err)
	}

	_, costOpt := storedFlags(t, m, "ws-c-out")
	if costOpt {
		t.Error("stored cost_optimize_routing=true: a re-registration retroactively granted routing consent")
	}
	if m.GetCostOptimizeRouting("ws-c-out") {
		t.Error("in-memory cost_optimize_routing was granted — RETURNING must reconcile it to the persisted false")
	}
}

// ⚠ THE COLD-REPLICA CASE, for both flags. A replica that never loaded the row cannot tell "new" from
// "unknown here", so it would apply the body value. The DB clause is the guarantor, and RETURNING
// must pull the replica's memory back to the persisted truth — the same protection #357 gave
// cache_poolable, now extended to the other two.
func TestRegisterWorkspace_ColdReplica_PreservesDistillAndRoutingConsent(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()

	mA := New(pool)
	if err := mA.RegisterWorkspace(ctx, Workspace{ID: "ws-cold-flags", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := mA.SetDistillPoolable(ctx, "ws-cold-flags", true); err != nil {
		t.Fatal(err)
	}
	if err := mA.SetCostOptimizeRouting(ctx, "ws-cold-flags", true); err != nil {
		t.Fatal(err)
	}

	// Replica B never ran LoadAll: its cache does not hold the row.
	mB := New(pool)
	if err := mB.RegisterWorkspace(ctx, Workspace{ID: "ws-cold-flags", Name: "x2"}); err != nil {
		t.Fatal(err)
	}

	distill, costOpt := storedFlags(t, mB, "ws-cold-flags")
	if !distill || !costOpt {
		t.Errorf("persisted flags distill=%v costOpt=%v, want true/true — a cold replica must not revoke consent it never knew about", distill, costOpt)
	}
	if !mB.GetDistillPoolable("ws-cold-flags") || !mB.GetCostOptimizeRouting("ws-cold-flags") {
		t.Error("replica B's in-memory flags were not reconciled to the persisted values")
	}
}

// The at-creation choice is UNAFFECTED: a NEW workspace still gets exactly what it asked for. Unlike
// cache_poolable (whose default is true, making an omitted field indistinguishable from an explicit
// false), these two default to FALSE — which coincides with Go's zero value — so a plain bool already
// expresses the full choice at creation and no *bool is needed at the wire.
func TestRegisterWorkspace_NewWorkspace_HonoursExplicitFlagsAtCreation(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	m := New(pool)

	if err := m.RegisterWorkspace(ctx, Workspace{
		ID: "ws-new-flags", Name: "n", DistillPoolable: true, CostOptimizeRouting: true,
	}); err != nil {
		t.Fatal(err)
	}
	if distill, costOpt := storedFlags(t, m, "ws-new-flags"); !distill || !costOpt {
		t.Errorf("new workspace stored distill=%v costOpt=%v, want true/true — an explicit choice at CREATION must be honoured", distill, costOpt)
	}

	// And silence at creation still means the safe default: false.
	if err := m.RegisterWorkspace(ctx, Workspace{ID: "ws-new-silent", Name: "s"}); err != nil {
		t.Fatal(err)
	}
	if distill, costOpt := storedFlags(t, m, "ws-new-silent"); distill || costOpt {
		t.Errorf("silent new workspace stored distill=%v costOpt=%v, want false/false", distill, costOpt)
	}
}
