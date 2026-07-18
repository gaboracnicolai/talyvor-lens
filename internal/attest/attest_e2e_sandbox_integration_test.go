package attest_test

// attest_e2e_sandbox_integration_test.go — THE test that makes H5 real: a served completion round-trips
// capture-extraction → artifact commit → Talyvor's REAL sandboxed reproduce → a talyvor_verified verdict.
// No fake verifier: the build runs in the actual buildverify docker sandbox. Requires real PG
// (LENS_TEST_DATABASE_URL) and a reachable container runtime — both present in CI; skips LOUDLY otherwise
// (the requireSandbox discipline: a missing sandbox must read as "NOT PROVEN HERE", never a silent pass).

import (
	"archive/tar"
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/attest"
	"github.com/talyvor/lens/internal/buildverify"
	"github.com/talyvor/lens/internal/outputverify"
)

func requireDockerSandbox(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		t.Skipf("⚠ E2E ATTESTATION NOT PROVEN HERE — no container runtime (docker) reachable (%v). "+
			"The commit→sandbox→talyvor_verified round-trip DID NOT RUN in this environment.", err)
	}
}

// The full producer story, with the REAL sandbox:
//  1. the gateway serves an anthropic completion whose text is a tiny Go program (no trailing newline —
//     the canonicalizer supplies it, exactly like the flagship writer);
//  2. capture derives output_content_sha256 from the SERVED envelope via CanonicalContentSHA256 and
//     records the verdict row (the same Writer the capture sink wires);
//  3. the producer commits the artifact: context manifest {go.mod} + output slot main.go — the committer
//     folds the CAPTURED content hash, ignoring any caller claim;
//  4. the attestor is handed the module exactly as the flagship writer materializes it on disk
//     (main.go = the canonical bytes) and REPRODUCES the build in the hardened docker sandbox;
//  5. a talyvor_verified 'compiled' verdict lands in k4_mechanical_verdicts for the OWNER.
func TestE2E_CommitSandboxAttest_TalyvorVerified(t *testing.T) {
	requireDockerSandbox(t)
	ctx := context.Background()
	pool := testPool(t)
	const ws, oid = "wsE2E", "oid-e2e-real-sandbox"
	const gomod = "module gen\ngo 1.21\n"

	// (1) The served envelope. The assistant text lacks the trailing newline; canonical form appends it.
	envelope := []byte(`{"id":"msg_e2e","type":"message","role":"assistant","content":[{"type":"text","text":"package main\n\nfunc main() {\n\tprintln(\"h5\")\n}"}],"usage":{"input_tokens":3,"output_tokens":9}}`)

	// (2) Capture: identity hash over the RAW envelope; content hash over the canonical bytes.
	canonical, ok := outputverify.CanonicalContent("anthropic", envelope)
	if !ok {
		t.Fatal("the served envelope must canonicalize")
	}
	contentSHA, _ := outputverify.CanonicalContentSHA256("anthropic", envelope)
	if inserted, err := outputverify.NewWriter(pool).Record(ctx, outputverify.VerdictRecord{
		OutputID: oid, WorkspaceID: ws, Model: "claude-x",
		Verdict: outputverify.VerdictUnverifiable, ConstraintKind: outputverify.KindNone,
		PromptSHA256:        outputverify.Sha256Hex([]byte("write me a main package")),
		ResponseSHA256:      outputverify.Sha256Hex(envelope),
		OutputContentSHA256: contentSHA,
	}); err != nil || !inserted {
		t.Fatalf("capture record: inserted=%v err=%v", inserted, err)
	}

	// (3) The producer commits the artifact. The committer folds the CAPTURED content hash into main.go.
	artifactSHA, committed, err := outputverify.NewArtifactCommitter(pool).Commit(ctx, oid, ws, "main.go",
		[]outputverify.ManifestEntry{{Path: "go.mod", ContentSHA256: outputverify.Sha256Hex([]byte(gomod))}})
	if err != nil || !committed {
		t.Fatalf("artifact commit: committed=%v err=%v", committed, err)
	}

	// (4) The module exactly as the flagship writer materializes it: main.go IS the canonical content.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ n, b string }{{"go.mod", gomod}, {"main.go", canonical}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.n, Mode: 0o644, Size: int64(len(f.b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.b)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// (5) REAL sandboxed reproduce (single platform to stay under the CI -race time cap; multi-platform
	// agreement is buildverify's own suite). The manifest must match the commitment, the build must run,
	// and the verdict must land as talyvor_verified 'compiled' for the owner.
	verifier := buildverify.NewVerifier(true, buildverify.WithPlatforms("linux/amd64"))
	res, err := attest.NewAttestor(pool, verifier, true).Attest(ctx, oid, buf.Bytes())
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	if res.Outcome == attest.OutcomeNotVerifiable {
		t.Fatalf("the sandbox refused to produce a verdict (%s) — the round-trip did not complete", res.Reason)
	}
	if res.Outcome != attest.OutcomeAttested || res.Verdict != string(buildverify.Compiled) || !res.Recorded {
		t.Fatalf("want ATTESTED/compiled/recorded; got outcome=%q verdict=%q recorded=%v reason=%q (artifact=%s)",
			res.Outcome, res.Verdict, res.Recorded, res.Reason, artifactSHA)
	}
	var verdict, source, owner string
	if err := pool.QueryRow(ctx,
		`SELECT verdict, verdict_source, workspace_id FROM k4_mechanical_verdicts WHERE output_id=$1 AND verdict_source='talyvor_verified'`, oid).
		Scan(&verdict, &source, &owner); err != nil {
		t.Fatalf("expected a talyvor_verified row: %v", err)
	}
	if verdict != outputverify.MechCompiled || owner != ws {
		t.Fatalf("row must be (compiled, owner=%s); got (%q, %q)", ws, verdict, owner)
	}
}
