package outputverify_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/outputverify"
)

// The commit producer folds the output's STORED response_sha256 (the served bytes) into the artifact — never
// a caller-supplied output hash — and only the producer may commit. This is the endpoint-level generation-time
// binding.
func TestArtifactCommit_FoldsServedResponseHash_OwnerBound(t *testing.T) {
	pool := ovTestPool(t)
	ctx := context.Background()
	const oid, owner = "oid-artifact-commit", "wsOwner"
	served := outputverify.Sha256Hex([]byte("the served code"))

	if _, err := outputverify.NewWriter(pool).Record(ctx, outputverify.VerdictRecord{
		OutputID: oid, WorkspaceID: owner, Model: "m", Verdict: outputverify.VerdictUnverifiable,
		ConstraintKind: outputverify.KindNone, PromptSHA256: "ph", ResponseSHA256: served,
	}); err != nil {
		t.Fatalf("record output: %v", err)
	}

	committer := outputverify.NewArtifactCommitter(pool)
	ctxManifest := []outputverify.ManifestEntry{{Path: "go.mod", ContentSHA256: "gomodhash"}}

	// A non-producer is refused.
	if _, _, err := committer.Commit(ctx, oid, "wsIntruder", "gen.go", ctxManifest); !errors.Is(err, outputverify.ErrNotOutputOwner) {
		t.Fatalf("a non-producer must be refused; got %v", err)
	}

	// The producer commits. A BOGUS output-slot claim in the manifest must be IGNORED — the served hash binds.
	bogus := append(append([]outputverify.ManifestEntry{}, ctxManifest...), outputverify.ManifestEntry{Path: "gen.go", ContentSHA256: "WORKSPACE_LIES"})
	got, committed, err := committer.Commit(ctx, oid, owner, "gen.go", bogus)
	if err != nil || !committed {
		t.Fatalf("producer commit must succeed; committed=%v err=%v", committed, err)
	}
	want := outputverify.CommitArtifactSHA256(ctxManifest, "gen.go", served) // folds the SERVED hash, not the lie
	if got != want {
		t.Fatalf("committed artifact must fold the served response_sha256, not the caller claim; got %s want %s", got, want)
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
