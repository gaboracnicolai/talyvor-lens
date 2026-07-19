package outputverify_test

// content_binding_moneyfrozen_integration_test.go — the content binding is DESCRIPTIVE ONLY. Establish real
// money state, then run all three content-binding paths — (1) capture-record with a content hash, (2) the
// artifact commit, (3) the attested verdict write — and prove every money column is BYTE-IDENTICAL before
// and after. None of the three may move a ledger row, a balance, supply, or burned totals.

import (
	"archive/tar"
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/attest"
	"github.com/talyvor/lens/internal/buildverify"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/outputverify"
)

// tarOf tars the given files (sorted paths, deterministic).
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type frozenFakeVerifier struct{}

func (frozenFakeVerifier) Verify(_ context.Context, _ string) buildverify.Result {
	return buildverify.Result{Verdict: buildverify.Compiled, Toolchain: "go1.25.11", Platform: "linux/amd64"}
}

func TestContentBinding_MoneyFrozen(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	ledger := mining.NewLedgerStore(pool)
	const ws, oid = "ws-content-money-frozen", "oid-content-money-frozen"

	// Establish REAL money state (held mint → settle), mirroring the attribution money-frozen proof.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.CreditHeldTx(ctx, tx, ws, 70_000, mining.TypePoolRoyaltyHeld, "seed held mint", nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.FinalizeHeldTxAs(ctx, tx2, ws, 70_000, mining.TypePoolRoyalty, "settle", nil); err != nil {
		_ = tx2.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	before := moneyDump(t, pool, ledger)
	if !strings.Contains(before, ws) {
		t.Fatalf("precondition: money state must be non-empty after the settle; dump:\n%s", before)
	}

	// (1) Capture-record with a content binding.
	code := "package gen\n"
	if inserted, err := outputverify.NewWriter(pool).Record(ctx, outputverify.VerdictRecord{
		OutputID: oid, WorkspaceID: ws, Model: "m",
		Verdict: outputverify.VerdictUnverifiable, ConstraintKind: outputverify.KindNone,
		PromptSHA256: "ph", ResponseSHA256: outputverify.Sha256Hex([]byte(`{"env":"mf"}`)),
		OutputContentSHA256: outputverify.Sha256Hex([]byte(code)),
	}); err != nil || !inserted {
		t.Fatalf("record: inserted=%v err=%v", inserted, err)
	}

	// (2) The artifact commit.
	gomod := "module gen\ngo 1.21\n"
	if _, committed, err := outputverify.NewArtifactCommitter(pool).Commit(ctx, oid, ws, "gen.go",
		[]outputverify.ManifestEntry{{Path: "go.mod", ContentSHA256: outputverify.Sha256Hex([]byte(gomod))}}); err != nil || !committed {
		t.Fatalf("commit: committed=%v err=%v", committed, err)
	}

	// (3) The attested verdict write (fake sandbox — the verdict row is the point, and it is NOT money).
	tarBytes := tarOf(t, map[string]string{"go.mod": gomod, "gen.go": code})
	res, err := attest.NewAttestor(pool, frozenFakeVerifier{}, true).Attest(ctx, oid, tarBytes)
	if err != nil || res.Outcome != attest.OutcomeAttested || !res.Recorded {
		t.Fatalf("attest: outcome=%q recorded=%v err=%v", res.Outcome, res.Recorded, err)
	}

	after := moneyDump(t, pool, ledger)
	if before != after {
		t.Fatalf("MONEY MOVED across the content-binding paths — capture/commit/attest must be descriptive only.\n--- BEFORE ---\n%s\n--- AFTER ---\n%s", before, after)
	}
}
