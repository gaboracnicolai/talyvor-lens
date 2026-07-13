package outputverify_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/outputverify"
)

// BREACH (CODE LOOP) — ownership binding: a workspace can report a mechanical verdict ONLY on an output it
// produced; workspace B is REJECTED on workspace A's output_id. Plus append-only dedup + the trust model.
func TestBreach_MechanicalOwnershipAndAppendOnly(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	// Establish ownership: workspace A produced output "oid-mech-A" (an ordinary K4 verdict row).
	ordWriter := outputverify.NewWriter(pool)
	if _, err := ordWriter.Record(ctx, outputverify.VerdictRecord{
		OutputID: "oid-mech-A", WorkspaceID: "ws-A", Model: "openai/gpt-4o",
		Verdict: outputverify.VerdictUnverifiable, ConstraintKind: outputverify.KindNone,
		PromptSHA256: outputverify.Sha256Hex([]byte("p")), ResponseSHA256: outputverify.Sha256Hex([]byte("r")),
	}); err != nil {
		t.Fatal(err)
	}

	mw := outputverify.NewMechanicalWriter(pool)

	// (1) The PRODUCER (ws-A) may self-report a FAILURE → owned + recorded.
	owned, recorded, err := mw.RecordMechanicalIfOwned(ctx, outputverify.MechanicalReport{
		OutputID: "oid-mech-A", WorkspaceID: "ws-A", Verdict: outputverify.MechCompileFailed, ExitCode: 1, Tool: "go build",
	})
	if err != nil || !owned || !recorded {
		t.Fatalf("producer self-report: owned=%v recorded=%v err=%v, want true/true/nil", owned, recorded, err)
	}

	// (2) WORKSPACE B CANNOT report on ws-A's output_id → REJECTED (owned=false, nothing recorded).
	ownedB, recordedB, err := mw.RecordMechanicalIfOwned(ctx, outputverify.MechanicalReport{
		OutputID: "oid-mech-A", WorkspaceID: "ws-B", Verdict: outputverify.MechTestsPassed, ExitCode: 0, Tool: "go test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ownedB || recordedB {
		t.Errorf("workspace B must NOT be able to report on ws-A's output_id; owned=%v recorded=%v", ownedB, recordedB)
	}

	// (3) APPEND-ONLY: a second report by the producer does NOT overwrite (recorded=false, still owned).
	owned2, recorded2, err := mw.RecordMechanicalIfOwned(ctx, outputverify.MechanicalReport{
		OutputID: "oid-mech-A", WorkspaceID: "ws-A", Verdict: outputverify.MechTestsPassed, ExitCode: 0, Tool: "go test",
	})
	if err != nil || !owned2 || recorded2 {
		t.Errorf("second report must dedup (append-only); owned=%v recorded=%v err=%v", owned2, recorded2, err)
	}
	// The stored verdict is still the FIRST one (compile_failed) — no mutation.
	var stored string
	if err := pool.QueryRow(ctx, `SELECT verdict FROM k4_mechanical_verdicts WHERE output_id=$1 AND verdict_source='self_reported'`, "oid-mech-A").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != outputverify.MechCompileFailed {
		t.Errorf("append-only: first verdict must win; stored=%q want %q", stored, outputverify.MechCompileFailed)
	}
}

// BREACH — HASHES ONLY + SELF ONLY: k4_mechanical_verdicts has no raw-content column and no counterparty column.
func TestBreach_MechanicalNoRawContent(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	_ = outputverify.NewMechanicalWriter(pool) // ensure migrated
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name='k4_mechanical_verdicts'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	// platform (0089) records the toolchain+GOOS/GOARCH of an attested verdict — metadata, not raw content.
	allowed := map[string]bool{"output_id": true, "workspace_id": true, "verdict": true, "exit_code": true, "tool": true, "reason": true, "verdict_source": true, "created_at": true, "platform": true}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		if !allowed[col] {
			t.Errorf("unexpected column %q — hashes/verdict only, self only", col)
		}
		for _, bad := range []string{"prompt_text", "response_text", "content", "counterparty", "other_workspace"} {
			if strings.Contains(col, bad) {
				t.Errorf("column %q looks like raw content / a counterparty — forbidden", col)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TRUST MODEL — the schema distinguishes self_reported source so H5 cannot confuse a pass with an attested
// one; only a self-reported FAILURE is slash-usable.
// The FULL IsSlashUsable truth table — the only gate that authorizes a burn.
func TestMechanical_TrustModel_SlashUsability(t *testing.T) {
	cases := []struct {
		verdict, source string
		want            bool
	}{
		// self_reported — credible against interest; a FAILURE is usable, a pass never is. Unchanged.
		{outputverify.MechCompileFailed, outputverify.SourceSelfReported, true},
		{outputverify.MechTestsFailed, outputverify.SourceSelfReported, true},
		{outputverify.MechCompiled, outputverify.SourceSelfReported, false},
		{outputverify.MechTestsPassed, outputverify.SourceSelfReported, false},
		// talyvor_verified — ONLY a compile failure is attestable.
		{outputverify.MechCompileFailed, outputverify.SourceTalyvorVerified, true},
		// THE LOAD-BEARING RULE: an attested TEST failure must NEVER be slash-usable (not reproducible).
		{outputverify.MechTestsFailed, outputverify.SourceTalyvorVerified, false},
		{outputverify.MechCompiled, outputverify.SourceTalyvorVerified, false},
		{outputverify.MechTestsPassed, outputverify.SourceTalyvorVerified, false},
		// unknown/forged sources are never usable.
		{outputverify.MechCompileFailed, "attested", false},
		{outputverify.MechCompileFailed, "hacker_says_so", false},
		{outputverify.MechTestsFailed, "", false},
	}
	for _, c := range cases {
		if got := outputverify.IsSlashUsable(c.verdict, c.source); got != c.want {
			t.Errorf("IsSlashUsable(%q, %q) = %v, want %v", c.verdict, c.source, got, c.want)
		}
	}
}

// DEFENSE IN DEPTH (real PG): the schema makes a talyvor_verified TEST row — most importantly the false-slash
// row (talyvor_verified, tests_failed) — and any unknown source UNREPRESENTABLE.
func TestAttested_CheckConstraints(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	ins := func(outputID, source, verdict string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO k4_mechanical_verdicts (output_id, workspace_id, verdict, exit_code, verdict_source)
			 VALUES ($1,'ws',$2,$3,$4)`, outputID, verdict, 1, source)
		return err
	}
	// ACCEPTED: attested compile verdicts + all self_reported verdicts.
	for i, ok := range []struct{ source, verdict string }{
		{outputverify.SourceTalyvorVerified, outputverify.MechCompileFailed},
		{outputverify.SourceTalyvorVerified, outputverify.MechCompiled},
		{outputverify.SourceSelfReported, outputverify.MechTestsFailed},
		{outputverify.SourceSelfReported, outputverify.MechCompileFailed},
	} {
		if err := ins("oid-ok-"+strconv.Itoa(i), ok.source, ok.verdict); err != nil {
			t.Errorf("(%s,%s) must be insertable; got %v", ok.source, ok.verdict, err)
		}
	}
	// REJECTED by CHECK: attested TEST verdicts (incl. the false-slash row) + unknown sources.
	for i, bad := range []struct{ source, verdict, why string }{
		{outputverify.SourceTalyvorVerified, outputverify.MechTestsFailed, "the false-slash row must be unrepresentable"},
		{outputverify.SourceTalyvorVerified, outputverify.MechTestsPassed, "attested is compile-only"},
		{"hacker_says_so", outputverify.MechCompileFailed, "unknown source must be rejected"},
		{"attested", outputverify.MechCompileFailed, "only the two known sources are allowed"},
	} {
		if err := ins("oid-bad-"+strconv.Itoa(i), bad.source, bad.verdict); err == nil {
			t.Errorf("(%s,%s) must be REJECTED by CHECK: %s", bad.source, bad.verdict, bad.why)
		}
	}
}

// The self-report endpoint's write path can ONLY produce verdict_source='self_reported' — a workspace can
// never forge an attested source for itself (the source is hard-coded, not caller-supplied).
func TestSelfReport_AlwaysWritesSelfReported(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	if _, err := outputverify.NewWriter(pool).Record(ctx, outputverify.VerdictRecord{
		OutputID: "oid-selfsrc", WorkspaceID: "ws-self", Model: "m",
		Verdict: outputverify.VerdictUnverifiable, ConstraintKind: outputverify.KindNone,
		PromptSHA256: outputverify.Sha256Hex([]byte("p")), ResponseSHA256: outputverify.Sha256Hex([]byte("r")),
	}); err != nil {
		t.Fatal(err)
	}
	owned, recorded, err := outputverify.NewMechanicalWriter(pool).RecordMechanicalIfOwned(ctx, outputverify.MechanicalReport{
		OutputID: "oid-selfsrc", WorkspaceID: "ws-self", Verdict: outputverify.MechCompileFailed, ExitCode: 1,
	})
	if err != nil || !owned || !recorded {
		t.Fatalf("self-report record: owned=%v recorded=%v err=%v", owned, recorded, err)
	}
	var src string
	if err := pool.QueryRow(ctx, `SELECT verdict_source FROM k4_mechanical_verdicts WHERE output_id=$1`, "oid-selfsrc").Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != outputverify.SourceSelfReported {
		t.Errorf("the self-report path must write self_reported; got %q — an attested source was forgeable", src)
	}
}
