package outputverify_test

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/outputverify"
)

// AttributionReader is the read-back for output_attributions (write-only until now). GetByOutput is
// owner-scoped (WHERE output_id AND workspace_id), so a foreign output resolves to zero rows — the handler
// renders that as 404 (no cross-tenant oracle). A single output can carry a pr AND a spec (PK includes
// target_kind), so GetByOutput returns a slice. ListByWorkspace returns only the caller's own.
func TestAttributionReader_ByOutputAndWorkspaceScope(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	if _, err := pool.Exec(ctx, `DELETE FROM output_attributions WHERE workspace_id = ANY($1)`,
		[]string{"wsA-attr", "wsB-attr"}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	w := outputverify.NewWriter(pool)
	aw := outputverify.NewAttributionWriter(pool)
	// wsA owns oid-A (attributes both a PR and a spec); wsB owns oid-B (a PR).
	if _, err := w.Record(ctx, rec("oid-A", "wsA-attr", outputverify.VerdictUnverifiable, "", outputverify.KindNone)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Record(ctx, rec("oid-B", "wsB-attr", outputverify.VerdictUnverifiable, "", outputverify.KindNone)); err != nil {
		t.Fatal(err)
	}
	mustAttr := func(oid, ws, kind, ref string) {
		t.Helper()
		owned, recorded, _, err := aw.RecordAttributionIfOwned(ctx, outputverify.Attribution{
			OutputID: oid, WorkspaceID: ws, TargetKind: kind, TargetRef: ref})
		if err != nil || !owned || !recorded {
			t.Fatalf("seed attr %s/%s: owned=%v recorded=%v err=%v", oid, kind, owned, recorded, err)
		}
	}
	mustAttr("oid-A", "wsA-attr", outputverify.AttrKindPR, "https://example/pr/1")
	mustAttr("oid-A", "wsA-attr", outputverify.AttrKindSpec, "spec://feature-x")
	mustAttr("oid-B", "wsB-attr", outputverify.AttrKindPR, "https://example/pr/9")

	r := outputverify.NewAttributionReader(pool)

	// GetByOutput: own output → its pr + spec (2 rows), each scoped to wsA, created_at surfaced.
	byOut, err := r.GetByOutput(ctx, "wsA-attr", "oid-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(byOut) != 2 {
		t.Fatalf("oid-A must carry 2 attributions (pr+spec), got %d", len(byOut))
	}
	for _, a := range byOut {
		if a.WorkspaceID != "wsA-attr" || a.OutputID != "oid-A" || a.CreatedAt.IsZero() {
			t.Errorf("attribution row wrong: %+v", a)
		}
	}

	// Cross-tenant: wsA asking for wsB's output → empty (→ handler 404, no oracle).
	foreign, err := r.GetByOutput(ctx, "wsA-attr", "oid-B")
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 0 {
		t.Fatalf("wsA must NOT read wsB's output attributions; got %d rows", len(foreign))
	}

	// ListByWorkspace: only wsA's own rows (both for oid-A); never wsB's.
	listA, err := r.ListByWorkspace(ctx, "wsA-attr", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 2 {
		t.Fatalf("wsA attribution list: got %d, want 2", len(listA))
	}
	for _, a := range listA {
		if a.WorkspaceID != "wsA-attr" {
			t.Fatalf("cross-tenant leak in workspace list: %+v", a)
		}
	}
}
