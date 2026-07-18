package outputverify_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/outputverify"
)

// The commit producer folds the output's STORED output_content_sha256 (the canonical served content — the
// bytes a buildable tree CAN carry) into the artifact — never a caller-supplied output hash — and only the
// producer may commit. This is the endpoint-level generation-time binding, re-pointed from the envelope hash
// (which no buildable tree could ever contain) to the content hash.
func TestArtifactCommit_FoldsServedContentHash_OwnerBound(t *testing.T) {
	pool := ovTestPool(t)
	ctx := context.Background()
	const oid, owner = "oid-artifact-commit", "wsOwner"
	envelope := outputverify.Sha256Hex([]byte(`{"content":[{"type":"text","text":"the served code"}]}`)) // identity
	content := outputverify.Sha256Hex([]byte("the served code\n"))                                       // canonical content — DIFFERENT bytes

	if _, err := outputverify.NewWriter(pool).Record(ctx, outputverify.VerdictRecord{
		OutputID: oid, WorkspaceID: owner, Model: "m", Verdict: outputverify.VerdictUnverifiable,
		ConstraintKind: outputverify.KindNone, PromptSHA256: "ph", ResponseSHA256: envelope,
		OutputContentSHA256: content,
	}); err != nil {
		t.Fatalf("record output: %v", err)
	}

	committer := outputverify.NewArtifactCommitter(pool)
	ctxManifest := []outputverify.ManifestEntry{{Path: "go.mod", ContentSHA256: "gomodhash"}}

	// A non-producer is refused.
	if _, _, err := committer.Commit(ctx, oid, "wsIntruder", "gen.go", ctxManifest); !errors.Is(err, outputverify.ErrNotOutputOwner) {
		t.Fatalf("a non-producer must be refused; got %v", err)
	}

	// The producer commits. A BOGUS output-slot claim in the manifest must be IGNORED — the served CONTENT
	// hash binds the slot, never the caller's claim and never the envelope hash.
	bogus := append(append([]outputverify.ManifestEntry{}, ctxManifest...), outputverify.ManifestEntry{Path: "gen.go", ContentSHA256: "WORKSPACE_LIES"})
	got, committed, err := committer.Commit(ctx, oid, owner, "gen.go", bogus)
	if err != nil || !committed {
		t.Fatalf("producer commit must succeed; committed=%v err=%v", committed, err)
	}
	want := outputverify.CommitArtifactSHA256(ctxManifest, "gen.go", content) // folds the CONTENT hash, not the lie
	if got != want {
		t.Fatalf("committed artifact must fold output_content_sha256, not the caller claim; got %s want %s", got, want)
	}
	if wrong := outputverify.CommitArtifactSHA256(ctxManifest, "gen.go", envelope); got == wrong {
		t.Fatal("committed artifact folded the ENVELOPE hash — the unbuildable old-world binding")
	}
	// It landed on the row, and a second commit is a no-op (append-once).
	var stored string
	_ = pool.QueryRow(ctx, `SELECT artifact_sha256 FROM k4_output_verdicts WHERE output_id=$1`, oid).Scan(&stored)
	if stored != want {
		t.Fatalf("stored artifact_sha256=%s, want %s", stored, want)
	}
	if _, committed2, _ := committer.Commit(ctx, oid, owner, "gen.go", ctxManifest); committed2 {
		t.Fatal("second commit must be a no-op (append-once)")
	}
}

// An output with NO content binding (output_content_sha256 NULL — pre-0098 rows, failed extraction,
// streaming) can never commit an artifact: there is nothing a buildable tree could match. Refused with the
// sentinel, row untouched.
func TestArtifactCommit_NoContentBinding_Refused(t *testing.T) {
	pool := ovTestPool(t)
	ctx := context.Background()
	const oid, owner = "oid-artifact-nocontent", "wsOwner"

	if _, err := outputverify.NewWriter(pool).Record(ctx, outputverify.VerdictRecord{
		OutputID: oid, WorkspaceID: owner, Model: "m", Verdict: outputverify.VerdictUnverifiable,
		ConstraintKind: outputverify.KindNone, PromptSHA256: "ph",
		ResponseSHA256: outputverify.Sha256Hex([]byte(`{"envelope":"only"}`)),
		// OutputContentSHA256 deliberately empty → NULL.
	}); err != nil {
		t.Fatalf("record output: %v", err)
	}

	committer := outputverify.NewArtifactCommitter(pool)
	_, _, err := committer.Commit(ctx, oid, owner, "gen.go",
		[]outputverify.ManifestEntry{{Path: "go.mod", ContentSHA256: "gomodhash"}})
	if !errors.Is(err, outputverify.ErrNoContentBinding) {
		t.Fatalf("a content-less output must be refused with ErrNoContentBinding; got %v", err)
	}
	var stored *string
	_ = pool.QueryRow(ctx, `SELECT artifact_sha256 FROM k4_output_verdicts WHERE output_id=$1`, oid).Scan(&stored)
	if stored != nil {
		t.Fatalf("no artifact may be recorded for a content-less output; got %q", *stored)
	}
}
