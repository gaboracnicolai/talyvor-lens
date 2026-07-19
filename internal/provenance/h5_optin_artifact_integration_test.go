package provenance_test

// h5_optin_artifact_integration_test.go — the money-path proof that an OPT-IN buildable-artifact
// commitment makes H5 ENFORCEABLE. A workspace commits artifact_sha256 (the manifest of its buildable
// module, output slot bound to the CAPTURED output_content_sha256 — the canonical served content), bonds
// the output, and Talyvor reproduces the build. A genuine compile_failed → a talyvor_verified row → the
// bond BURNS — even though the workspace never self-reported a failure. Without the seam the attestor
// binds the JSON envelope and refuses everything; with the content binding a real tree can match.

import (
	"archive/tar"
	"bytes"
	"context"
	"testing"

	"github.com/talyvor/lens/internal/attest"
	"github.com/talyvor/lens/internal/buildverify"
	"github.com/talyvor/lens/internal/outputverify"
)

type fakeVerifier struct{ r buildverify.Result }

func (f fakeVerifier) Verify(_ context.Context, _ string) buildverify.Result { return f.r }

func compileFailedVerifier() fakeVerifier {
	return fakeVerifier{buildverify.Result{Verdict: buildverify.CompileFailed, Reason: "gen.go:2: missing return", Toolchain: "go1.25.11", Platform: "linux/amd64,linux/arm64"}}
}

func compiledVerifier() fakeVerifier {
	return fakeVerifier{buildverify.Result{Verdict: buildverify.Compiled, Toolchain: "go1.25.11", Platform: "linux/amd64,linux/arm64"}}
}

// tarModule tars {go.mod, gen.go:code}.
func tarModule(t *testing.T, gomod, code string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ n, b string }{{"go.mod", gomod}, {"gen.go", code}} {
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
	return buf.Bytes()
}

// TestH5OptIn_ServedDifferentBytes_Refused is the SOUNDNESS proof (the mutation target). A workspace SERVES
// broken code X but commits an artifact CLAIMING its output is a different, compiling Y, then supplies the
// compiling Y tree at attest time. Because CommitArtifactSHA256 FORCES the output slot to the captured
// output_content_sha256 (H(canonical X)), the committed artifact binds X — so the Y tree fails the manifest
// binding and is REFUSED. The workspace cannot escape by substituting a compiling tree for what it served.
// (Neuter the generation-time forcing and this REFUSAL becomes an ATTESTATION — the unsound
// bond-time-supply path.)
func TestH5OptIn_ServedDifferentBytes_Refused(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	const ws, oid = "wsH5diff", "oid-h5-served-diff"

	const gomod = "module ex\ngo 1.21\n"
	const brokenX = "package ex\nfunc F() int { return }\n" // ACTUALLY served (canonical content) — does not compile
	const goodY = "package ex\nfunc F() int { return 0 }\n" // the compiling tree the workspace SUBSTITUTES
	servedContentSHA := outputverify.Sha256Hex([]byte(brokenX))
	envelopeSHA := outputverify.Sha256Hex([]byte(`{"content":[{"type":"text","text":"` + brokenX + `"}]}`)) // identity, distinct

	// The workspace commits a manifest CLAIMING gen.go = goodY. CommitArtifactSHA256 ignores the claim and
	// binds the served H(canonical brokenX) — the sound path.
	claimed := []outputverify.ManifestEntry{
		{Path: "go.mod", ContentSHA256: outputverify.Sha256Hex([]byte(gomod))},
		{Path: "gen.go", ContentSHA256: outputverify.Sha256Hex([]byte(goodY))}, // bogus claim
	}
	artifactSHA := outputverify.CommitArtifactSHA256(claimed, "gen.go", servedContentSHA)

	if _, err := pool.Exec(ctx,
		`INSERT INTO k4_output_verdicts (output_id, workspace_id, model, verdict, reason, constraint_kind, prompt_sha256, response_sha256, output_content_sha256, artifact_sha256, artifact_output_path)
		 VALUES ($1,$2,'m','unverifiable','','none','ph',$3,$4,$5,'gen.go') ON CONFLICT DO NOTHING`,
		oid, ws, envelopeSHA, servedContentSHA, artifactSHA); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The workspace supplies the COMPILING Y tree. A verifier that would say "compiled" — but the binding must
	// refuse BEFORE any verdict, because the tree's output slot (H(goodY)) ≠ the served output (H(brokenX)).
	res, err := attest.NewAttestor(pool, compiledVerifier(), true).Attest(ctx, oid, tarModule(t, gomod, goodY))
	if err != nil {
		t.Fatalf("attest err: %v", err)
	}
	if res.Outcome != attest.OutcomeRefused {
		t.Fatalf("a served-different tree MUST be refused (bound to the served output); got outcome=%q — the generation-time binding is not forcing the served slot", res.Outcome)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM k4_mechanical_verdicts WHERE output_id=$1 AND verdict_source='talyvor_verified'`, oid).Scan(&n)
	if n != 0 {
		t.Fatalf("no talyvor_verified row may exist for a served-different tree; got %d", n)
	}
}

// TestH5OptIn_NonOptedOutput_StillBindsResponse is the NON-REGRESSION guard: an output with NO artifact
// commitment (artifact_sha256 NULL) still uses the legacy binding (sha256(tree) == response_sha256) and still
// refuses a buildable tree — existing bonds are byte-for-byte unchanged and still fail-safe.
func TestH5OptIn_NonOptedOutput_StillBindsResponse(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	const ws, oid = "wsH5legacy", "oid-h5-legacy"
	// Non-opted: response_sha256 is the JSON envelope hash; artifact columns are NULL.
	if _, err := pool.Exec(ctx,
		`INSERT INTO k4_output_verdicts (output_id, workspace_id, model, verdict, reason, constraint_kind, prompt_sha256, response_sha256)
		 VALUES ($1,$2,'m','unverifiable','','none','ph',$3) ON CONFLICT DO NOTHING`,
		oid, ws, outputverify.Sha256Hex([]byte(`{"choices":[{"message":{"content":"..."}}]}`))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A buildable module tar ≠ the JSON envelope → legacy binding refuses (unchanged fail-open).
	res, _ := attest.NewAttestor(pool, compileFailedVerifier(), true).Attest(ctx, oid, tarModule(t, "module ex\ngo 1.21\n", "package ex\n"))
	if res.Outcome != attest.OutcomeRefused {
		t.Fatalf("a non-opted output must still bind response_sha256 and REFUSE a buildable tree; got %q", res.Outcome)
	}
}

// buildableModule tars a {go.mod, <outputPath>:<outputCode>} module and returns the tar plus the commitment
// values a genuine opt-in produces under the CONTENT binding: output_content_sha256 = H(the canonical served
// content — the code file's exact bytes), and artifact_sha256 = the generation-time-bound manifest folding
// that content hash into the output slot. (response_sha256 — the envelope identity — is separate and
// distinct; the tree never contains it.)
func buildableModule(t *testing.T, gomod, outputPath, outputCode string) (tarBytes []byte, contentSHA, artifactSHA string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", gomod)
	write(outputPath, outputCode)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	contentSHA = outputverify.Sha256Hex([]byte(outputCode)) // canonical served content = the code file's bytes
	artifactSHA = outputverify.CommitArtifactSHA256(
		[]outputverify.ManifestEntry{{Path: "go.mod", ContentSHA256: outputverify.Sha256Hex([]byte(gomod))}},
		outputPath, contentSHA)
	return buf.Bytes(), contentSHA, artifactSHA
}

func TestH5OptIn_BoundBrokenModule_TalyvorVerifiedBurn(t *testing.T) {
	ctx := context.Background()
	pool := bondTestPool(t)
	const ws, oid = "wsH5", "oid-h5-optin"

	// A module whose generated file does NOT compile (missing return value).
	tarBytes, contentSHA, artifactSHA := buildableModule(t, "module ex\ngo 1.21\n", "gen.go",
		"package ex\nfunc F() int { return }\n")

	seedBalance(t, pool, ws, 5_000_000)
	if _, err := pool.Exec(ctx,
		`INSERT INTO k4_output_verdicts (output_id, workspace_id, model, verdict, reason, constraint_kind, prompt_sha256, response_sha256, output_content_sha256, artifact_sha256, artifact_output_path)
		 VALUES ($1,$2,'m','unverifiable','','none','ph',$3,$4,$5,'gen.go') ON CONFLICT DO NOTHING`,
		oid, ws, outputverify.Sha256Hex([]byte(`{"envelope":"h5-optin"}`)), contentSHA, artifactSHA); err != nil {
		t.Fatalf("seed opted-in output: %v", err)
	}

	// Bond 1_000_000 µLENS on the output.
	m := newManager(pool)
	bondID, created, err := m.CreateBond(ctx, ws, oid, 1_000_000)
	if err != nil || !created {
		t.Fatalf("CreateBond: created=%v err=%v", created, err)
	}
	if bal, lk := balanceOf(t, pool, ws); bal != 4_000_000 || lk != 1_000_000 {
		t.Fatalf("after lock: bal=%d locked=%d, want 4_000_000/1_000_000", bal, lk)
	}

	// Talyvor reproduces the build → compile_failed → a talyvor_verified row for the OWNER. (Fake verifier so
	// no Docker is needed; buildverify's own real-sandbox behavior is tested separately.)
	res, err := attest.NewAttestor(pool, compileFailedVerifier(), true).Attest(ctx, oid, tarBytes)
	if err != nil || res.Outcome != attest.OutcomeAttested || !res.Recorded {
		t.Fatalf("opted-in bound compile_failed must ATTEST + record; outcome=%q recorded=%v err=%v", res.Outcome, res.Recorded, err)
	}
	var v, s, vws string
	if err := pool.QueryRow(ctx,
		`SELECT verdict, verdict_source, workspace_id FROM k4_mechanical_verdicts WHERE output_id=$1 AND verdict_source='talyvor_verified'`, oid).
		Scan(&v, &s, &vws); err != nil {
		t.Fatalf("expected a talyvor_verified row: %v", err)
	}
	if v != outputverify.MechCompileFailed || vws != ws {
		t.Fatalf("row must be (compile_failed, owner=%s); got (%q, %q)", ws, v, vws)
	}

	// Past the appeal deadline → the attested compile_failed SLASHES the bond: the locked collateral BURNS.
	pushDeadlinePast(t, pool, bondID)
	outcome, err := m.SettleBond(ctx, bondID)
	if err != nil || outcome != "slashed" {
		t.Fatalf("SettleBond must slash on the attested compile_failed; outcome=%q err=%v", outcome, err)
	}
	if bal, lk := balanceOf(t, pool, ws); bal != 4_000_000 || lk != 0 {
		t.Fatalf("after burn: bal=%d locked=%d, want 4_000_000/0 (locked collateral burned)", bal, lk)
	}
}
