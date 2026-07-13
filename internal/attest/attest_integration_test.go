package attest_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/attest"
	"github.com/talyvor/lens/internal/buildverify"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/outputverify"
	"github.com/talyvor/lens/migrations"
)

var migrateOnce sync.Once

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG attest test")
	}
	ctx := context.Background()
	migrateOnce.Do(func() {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	})
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeVerifier returns a fixed buildverify.Result — its domain is compiled|compile_failed|not_verifiable, so
// an attested TEST verdict is structurally impossible.
type fakeVerifier struct{ r buildverify.Result }

func (f fakeVerifier) Verify(_ context.Context, _ string) buildverify.Result { return f.r }

// makeTar builds a tiny module tarball + returns (bytes, sha256hex(bytes)).
func makeTar(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range map[string]string{"go.mod": "module ok\ngo 1.21\n", "main.go": "package main\nfunc main(){}\n"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	b := buf.Bytes()
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

// seedOutput records a k4_output_verdicts row so output_id binds (response_sha256) to owner.
func seedOutput(t *testing.T, pool *pgxpool.Pool, outputID, owner, responseSHA string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO k4_output_verdicts (output_id, workspace_id, model, verdict, reason, constraint_kind, prompt_sha256, response_sha256)
		 VALUES ($1,$2,'m','unverifiable','','none','ph',$3) ON CONFLICT DO NOTHING`, outputID, owner, responseSHA)
	if err != nil {
		t.Fatal(err)
	}
}

func attestorWith(pool *pgxpool.Pool, verdict buildverify.Verdict, enabled bool) *attest.Attestor {
	return attest.NewAttestor(pool, fakeVerifier{buildverify.Result{Verdict: verdict, Toolchain: "go1.25.11", Platform: "linux/amd64,linux/arm64"}}, enabled)
}

func rowFor(t *testing.T, pool *pgxpool.Pool, outputID string) (verdict, source, platform, ws string, found bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT verdict, verdict_source, platform, workspace_id FROM k4_mechanical_verdicts WHERE output_id=$1 AND verdict_source='talyvor_verified'`, outputID).
		Scan(&verdict, &source, &platform, &ws)
	if err != nil {
		return "", "", "", "", false
	}
	return verdict, source, platform, ws, true
}

// BOUND compile_failed → a talyvor_verified row is written for the OWNER, with the platform recorded.
func TestAttest_BoundCompileFailed_Records(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	tarBytes, sha := makeTar(t)
	seedOutput(t, pool, "oid-cf", "ws-owner", sha)
	res, err := attestorWith(pool, buildverify.CompileFailed, true).Attest(ctx, "oid-cf", tarBytes)
	if err != nil || res.Outcome != attest.OutcomeAttested || !res.Recorded {
		t.Fatalf("bound compile_failed must record; outcome=%q recorded=%v err=%v", res.Outcome, res.Recorded, err)
	}
	v, s, plat, ws, ok := rowFor(t, pool, "oid-cf")
	if !ok || v != outputverify.MechCompileFailed || s != outputverify.SourceTalyvorVerified {
		t.Errorf("row must be (compile_failed, talyvor_verified); got %q/%q", v, s)
	}
	if ws != "ws-owner" {
		t.Errorf("row must be attributed to the OWNER, not a caller; got %q", ws)
	}
	if plat == "" {
		t.Error("attested row must record the platform")
	}
}

// not_verifiable → NO row (fail open); the bond stays on the self-reported path.
func TestAttest_NotVerifiable_NoRow(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	tarBytes, sha := makeTar(t)
	seedOutput(t, pool, "oid-nv", "ws-owner", sha)
	res, _ := attestorWith(pool, buildverify.NotVerifiable, true).Attest(ctx, "oid-nv", tarBytes)
	if res.Outcome != attest.OutcomeNotVerifiable || res.Recorded {
		t.Errorf("not_verifiable must record NOTHING; outcome=%q recorded=%v", res.Outcome, res.Recorded)
	}
	if _, _, _, _, ok := rowFor(t, pool, "oid-nv"); ok {
		t.Error("not_verifiable must not write a row")
	}
}

// UNBOUND tree (hash mismatch) → REFUSE, no row. THE SOURCE-PROVENANCE GUARANTEE.
func TestAttest_UnboundTree_Refused(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	tarBytes, _ := makeTar(t)
	seedOutput(t, pool, "oid-unbound", "ws-owner", "0000000000000000000000000000000000000000000000000000000000000000")
	res, _ := attestorWith(pool, buildverify.CompileFailed, true).Attest(ctx, "oid-unbound", tarBytes)
	if res.Outcome != attest.OutcomeRefused {
		t.Errorf("an unbound tree must be REFUSED; got %q", res.Outcome)
	}
	if _, _, _, _, ok := rowFor(t, pool, "oid-unbound"); ok {
		t.Error("an unbound tree must never write a verdict")
	}
}

// Unknown output → refuse. Disabled → refuse. Neither writes a row.
func TestAttest_UnknownAndDisabled_Refused(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	tarBytes, sha := makeTar(t)
	// unknown output_id (not seeded)
	if res, _ := attestorWith(pool, buildverify.CompileFailed, true).Attest(ctx, "oid-missing", tarBytes); res.Outcome != attest.OutcomeRefused {
		t.Errorf("unknown output must refuse; got %q", res.Outcome)
	}
	// disabled attestor
	seedOutput(t, pool, "oid-disabled", "ws-o", sha)
	if res, _ := attestorWith(pool, buildverify.CompileFailed, false).Attest(ctx, "oid-disabled", tarBytes); res.Outcome != attest.OutcomeRefused || res.Recorded {
		t.Errorf("disabled attestor must refuse + record nothing; got %q recorded=%v", res.Outcome, res.Recorded)
	}
}

// The attested writer can only ever produce compiled|compile_failed — NEVER a test verdict (buildverify's
// domain excludes tests, and the 0087 CHECK is the backstop). Prove a compiled maps to compiled.
func TestAttest_OnlyCompileVerdicts(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	tarBytes, sha := makeTar(t)
	seedOutput(t, pool, "oid-compiled", "ws-o", sha)
	if res, err := attestorWith(pool, buildverify.Compiled, true).Attest(ctx, "oid-compiled", tarBytes); err != nil || res.Verdict != "compiled" {
		t.Fatalf("compiled must map to compiled; got %q err=%v", res.Verdict, err)
	}
	v, _, _, _, _ := rowFor(t, pool, "oid-compiled")
	if v != outputverify.MechCompiled {
		t.Errorf("attested row must be compiled; got %q", v)
	}
}
