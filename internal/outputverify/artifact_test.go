package outputverify

import "testing"

// ManifestHash is a canonical, order-independent hash over (path → content-sha256) entries.
func TestManifestHash_OrderIndependentAndSensitive(t *testing.T) {
	a := []ManifestEntry{{"go.mod", "h1"}, {"gen.go", "h2"}}
	b := []ManifestEntry{{"gen.go", "h2"}, {"go.mod", "h1"}} // reordered
	if ManifestHash(a) != ManifestHash(b) {
		t.Fatal("manifest hash must be order-independent (sorted by path)")
	}
	c := []ManifestEntry{{"go.mod", "h1"}, {"gen.go", "h2-DIFFERENT"}}
	if ManifestHash(a) == ManifestHash(c) {
		t.Fatal("a changed content hash must change the manifest hash")
	}
}

// THE SOUNDNESS PROPERTY (the mutation target): CommitArtifactSHA256 binds the OUTPUT SLOT to the
// actually-served response hash, and IGNORES any output-slot hash the workspace tries to supply. Two
// different served outputs (same context) MUST yield different commitments; a workspace's claimed output
// slot MUST NOT change the commitment.
func TestCommitArtifactSHA256_BindsServedOutputNotWorkspaceClaim(t *testing.T) {
	ctx := []ManifestEntry{{"go.mod", "gomodhash"}}
	const servedX = "aaaa_served_X"
	const servedY = "bbbb_served_Y"

	// (1) The served bytes are bound: different served output → different commitment.
	if CommitArtifactSHA256(ctx, "gen.go", servedX) == CommitArtifactSHA256(ctx, "gen.go", servedY) {
		t.Fatal("served output must be bound — different served bytes must change artifact_sha256")
	}

	// (2) A workspace's CLAIMED output-slot hash is overridden by the served hash. Supplying entries that
	//     include a bogus gen.go claim must produce the SAME commitment as supplying only the context — the
	//     served hash wins. (Under the bond-time-supply mutation, the claim would leak in and these differ.)
	withBogusClaim := []ManifestEntry{{"go.mod", "gomodhash"}, {"gen.go", "ZZZZ_workspace_claims_this"}}
	if CommitArtifactSHA256(withBogusClaim, "gen.go", servedX) != CommitArtifactSHA256(ctx, "gen.go", servedX) {
		t.Fatal("a workspace-claimed output slot must be IGNORED — only the served hash binds the output slot")
	}
	// And it must equal a manifest that explicitly places the served hash at gen.go.
	want := ManifestHash([]ManifestEntry{{"go.mod", "gomodhash"}, {"gen.go", servedX}})
	if CommitArtifactSHA256(withBogusClaim, "gen.go", servedX) != want {
		t.Fatal("commitment must equal ManifestHash(context ∪ {output_path: served})")
	}
}
