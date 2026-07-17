package outputverify_test

// attribution_integration_test.go — POST /v1/outputs/{output_id}/attribution store proofs (real PG).
//
// Attribution ≠ settlement: the PRODUCING workspace attributes an output IT OWNS to a PR or spec.
// Ownership is the EXISTS gate against k4_output_verdicts (the producer registry) — the exact shape
// mechanical.go uses. output_id is content-addressed over workspace_id (identity.go:DeriveOutputID),
// so a workspace can never present an id it did not produce. No amount column exists on this path.

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/outputverify"
)

// PROPERTY 1 — CROSS-TENANT (the load-bearing anti-spoof): a workspace attributing ANOTHER
// workspace's output_id is REFUSED (owned=false) and NO row is written under it.
func TestAttribution_CrossTenant_Refused(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)

	// ws-A produced oid-attr-A (establish ownership via an ordinary K4 verdict row).
	if _, err := outputverify.NewWriter(pool).Record(ctx, outputverify.VerdictRecord{
		OutputID: "oid-attr-A", WorkspaceID: "ws-A", Model: "openai/gpt-4o",
		Verdict: outputverify.VerdictUnverifiable, ConstraintKind: outputverify.KindNone,
		PromptSHA256: outputverify.Sha256Hex([]byte("p")), ResponseSHA256: outputverify.Sha256Hex([]byte("r")),
	}); err != nil {
		t.Fatal(err)
	}

	aw := outputverify.NewAttributionWriter(pool)

	// ws-B (NOT the producer) tries to attribute ws-A's output → owned=false, nothing recorded.
	owned, recorded, conflict, err := aw.RecordAttributionIfOwned(ctx, outputverify.Attribution{
		OutputID: "oid-attr-A", WorkspaceID: "ws-B", TargetKind: "pr", TargetRef: "https://example/pull/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if owned || recorded || conflict {
		t.Fatalf("CROSS-TENANT: ws-B must NOT attribute ws-A's output; owned=%v recorded=%v conflict=%v", owned, recorded, conflict)
	}

	// And NO row exists under (oid-attr-A, ws-B) — the write must not have landed. This is the
	// assertion the mutation (drop the EXISTS gate) makes fail.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM output_attributions WHERE output_id='oid-attr-A' AND workspace_id='ws-B'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("CROSS-TENANT WRITE LANDED: %d attribution row(s) under a non-producer workspace — the ownership gate leaked", n)
	}
}
