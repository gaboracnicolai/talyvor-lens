package provenance_test

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/provenance"
)

// BondReader is the read-back for provenance bonds (write-only until now). It must be workspace-scoped:
// ListByWorkspace returns only the caller's bonds, and GetByID is owner-scoped so a FOREIGN bond_id is
// not-found (the handler turns that into 404, not 403 — no cross-tenant oracle). appeal_deadline MUST be
// surfaced (a bond UI is useless without knowing when the burn finalizes).
func TestBondReader_WorkspaceScopedAndGetByID(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	// Clean slate for the two workspaces (provenance_bonds permits UPDATE/DELETE — it is not append-only).
	if _, err := pool.Exec(ctx, `DELETE FROM provenance_bonds WHERE workspace_id = ANY($1)`,
		[]string{"wsA-bond", "wsB-bond"}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	m := newManager(pool)
	seedBalance(t, pool, "wsA-bond", 50_000_000)
	seedBalance(t, pool, "wsB-bond", 50_000_000)
	seedOwnedOutput(t, pool, "wsA-bond", "oid-A1")
	seedOwnedOutput(t, pool, "wsB-bond", "oid-B1")

	bondA, createdA, err := m.CreateBond(ctx, "wsA-bond", "oid-A1", 1_000_000)
	if err != nil || !createdA {
		t.Fatalf("CreateBond A: created=%v err=%v", createdA, err)
	}
	bondB, _, err := m.CreateBond(ctx, "wsB-bond", "oid-B1", 2_000_000)
	if err != nil {
		t.Fatalf("CreateBond B: %v", err)
	}

	r := provenance.NewBondReader(pool)

	// ListByWorkspace: only wsA's bond, with appeal_deadline + amount surfaced.
	listA, err := r.ListByWorkspace(ctx, "wsA-bond", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].BondID != bondA {
		t.Fatalf("wsA list must be exactly [%s], got %+v", bondA, listA)
	}
	if listA[0].AmountULens != 1_000_000 {
		t.Errorf("amount_ulens: got %d want 1000000", listA[0].AmountULens)
	}
	if listA[0].AppealDeadline.IsZero() {
		t.Error("appeal_deadline must be surfaced (a UI is useless without it)")
	}
	if listA[0].Status != "active" {
		t.Errorf("status: got %q want active", listA[0].Status)
	}

	// GetByID owner-scoped: own bond found; a FOREIGN bond_id is not-found (→ handler 404, no oracle).
	got, ok, err := r.GetByID(ctx, "wsA-bond", bondA)
	if err != nil || !ok {
		t.Fatalf("GetByID own bond: ok=%v err=%v", ok, err)
	}
	if got.OutputID != "oid-A1" || got.AppealDeadline.IsZero() {
		t.Errorf("own bond fields wrong: %+v", got)
	}
	_, okForeign, err := r.GetByID(ctx, "wsA-bond", bondB)
	if err != nil {
		t.Fatal(err)
	}
	if okForeign {
		t.Fatalf("cross-tenant: wsA must NOT resolve wsB's bond %s — it must be not-found (404, not 403)", bondB)
	}
}
