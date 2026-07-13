package outputverify_test

import (
	"context"
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
	allowed := map[string]bool{"output_id": true, "workspace_id": true, "verdict": true, "exit_code": true, "tool": true, "reason": true, "verdict_source": true, "created_at": true}
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
func TestMechanical_TrustModel_SlashUsability(t *testing.T) {
	// self-reported failures are usable as slash evidence (credible against interest).
	if !outputverify.IsSlashUsable(outputverify.MechCompileFailed, outputverify.SourceSelfReported) {
		t.Error("a self-reported compile_failed must be slash-usable")
	}
	if !outputverify.IsSlashUsable(outputverify.MechTestsFailed, outputverify.SourceSelfReported) {
		t.Error("a self-reported tests_failed must be slash-usable")
	}
	// self-reported PASSES prove nothing — NEVER slash/release evidence.
	if outputverify.IsSlashUsable(outputverify.MechCompiled, outputverify.SourceSelfReported) {
		t.Error("a self-reported compiled (pass) must NOT be slash-usable — a liar always claims success")
	}
	if outputverify.IsSlashUsable(outputverify.MechTestsPassed, outputverify.SourceSelfReported) {
		t.Error("a self-reported tests_passed (pass) must NOT be slash-usable")
	}
	// a failure from an UNKNOWN (non-self-reported) source is not usable via this rule.
	if outputverify.IsSlashUsable(outputverify.MechCompileFailed, "attested") {
		t.Error("only self_reported is defined today; other sources must not be treated as usable by this rule")
	}
}
