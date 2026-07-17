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

// seedProducer makes ws the producer of oid (an ordinary K4 verdict row = the ownership anchor).
func seedProducer(t *testing.T, pool interface {
	Record(context.Context, outputverify.VerdictRecord) (bool, error)
}, oid, ws, salt string) {
	t.Helper()
	if _, err := pool.Record(context.Background(), outputverify.VerdictRecord{
		OutputID: oid, WorkspaceID: ws, Model: "openai/gpt-4o",
		Verdict: outputverify.VerdictUnverifiable, ConstraintKind: outputverify.KindNone,
		PromptSHA256: outputverify.Sha256Hex([]byte("p" + salt)), ResponseSHA256: outputverify.Sha256Hex([]byte("r" + salt)),
	}); err != nil {
		t.Fatalf("seed producer %s/%s: %v", oid, ws, err)
	}
}

// PROPERTY 2 — OWNERSHIP HAPPY PATH: the producing workspace attributes its OWN output → owned+recorded;
// the stored row carries the CALLER's workspace_id.
func TestAttribution_OwnershipHappyPath(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	seedProducer(t, outputverify.NewWriter(pool), "oid-attr-happy", "ws-A", "happy")

	owned, recorded, conflict, err := outputverify.NewAttributionWriter(pool).RecordAttributionIfOwned(ctx,
		outputverify.Attribution{OutputID: "oid-attr-happy", WorkspaceID: "ws-A", TargetKind: "pr", TargetRef: "spec://feature-x"})
	if err != nil || !owned || !recorded || conflict {
		t.Fatalf("producer attribution: owned=%v recorded=%v conflict=%v err=%v, want true/true/false/nil", owned, recorded, conflict, err)
	}
	var ws, kind, ref string
	if err := pool.QueryRow(ctx,
		`SELECT workspace_id, target_kind, target_ref FROM output_attributions WHERE output_id='oid-attr-happy'`).Scan(&ws, &kind, &ref); err != nil {
		t.Fatal(err)
	}
	if ws != "ws-A" || kind != "pr" || ref != "spec://feature-x" {
		t.Errorf("stored row = (%q,%q,%q), want (ws-A, pr, spec://feature-x)", ws, kind, ref)
	}
}

// PROPERTY 3 — IDEMPOTENCY: an IDENTICAL re-post is an append-only no-op → recorded:false, exactly one
// row (never a conflict). The PK (output_id, workspace_id, target_kind) carries every identity (SEC-11).
func TestAttribution_Idempotent_RePost(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	seedProducer(t, outputverify.NewWriter(pool), "oid-attr-idem", "ws-A", "idem")
	aw := outputverify.NewAttributionWriter(pool)
	a := outputverify.Attribution{OutputID: "oid-attr-idem", WorkspaceID: "ws-A", TargetKind: "spec", TargetRef: "spec://v1"}

	if owned, recorded, conflict, err := aw.RecordAttributionIfOwned(ctx, a); err != nil || !owned || !recorded || conflict {
		t.Fatalf("first post: owned=%v recorded=%v conflict=%v err=%v, want true/true/false/nil", owned, recorded, conflict, err)
	}
	// Identical re-post → idempotent no-op: still owned, recorded=false, NOT a conflict.
	if owned, recorded, conflict, err := aw.RecordAttributionIfOwned(ctx, a); err != nil || !owned || recorded || conflict {
		t.Fatalf("identical re-post: owned=%v recorded=%v conflict=%v err=%v, want true/false/false/nil (idempotent)", owned, recorded, conflict, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM output_attributions WHERE output_id='oid-attr-idem'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("idempotency: %d rows for the same (output,workspace,kind), want exactly 1", n)
	}
}

// PROPERTY 4 — APPEND-ONLY CONFLICT (D1): the same output+kind with a DIFFERENT target_ref is REFUSED
// (conflict=true, recorded=false, still owned); the ORIGINAL row is unchanged (first-wins), one row.
func TestAttribution_AppendOnlyConflict(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	seedProducer(t, outputverify.NewWriter(pool), "oid-attr-conflict", "ws-A", "conflict")
	aw := outputverify.NewAttributionWriter(pool)

	if owned, recorded, conflict, err := aw.RecordAttributionIfOwned(ctx, outputverify.Attribution{
		OutputID: "oid-attr-conflict", WorkspaceID: "ws-A", TargetKind: "pr", TargetRef: "pr://1"}); err != nil || !owned || !recorded || conflict {
		t.Fatalf("first attribution: owned=%v recorded=%v conflict=%v err=%v, want true/true/false/nil", owned, recorded, conflict, err)
	}
	// Same output+kind, DIFFERENT target_ref → conflict (append-only first-wins).
	owned, recorded, conflict, err := aw.RecordAttributionIfOwned(ctx, outputverify.Attribution{
		OutputID: "oid-attr-conflict", WorkspaceID: "ws-A", TargetKind: "pr", TargetRef: "pr://2"})
	if err != nil {
		t.Fatal(err)
	}
	if !owned || recorded || !conflict {
		t.Fatalf("CONFLICT: a different target_ref for the same kind must be refused; owned=%v recorded=%v conflict=%v, want true/false/true", owned, recorded, conflict)
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT target_ref FROM output_attributions WHERE output_id='oid-attr-conflict' AND target_kind='pr'`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref != "pr://1" {
		t.Fatalf("append-only: the ORIGINAL target_ref must survive; got %q, want pr://1", ref)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM output_attributions WHERE output_id='oid-attr-conflict'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("a conflicting re-attribution must not add a row; got %d, want 1", n)
	}
}
