package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
)

// THE AUDIT EXPORT'S `cached` FIELD IS READ FROM A COLUMN NOTHING WRITES.
//
// token_events.cached is BOOLEAN NOT NULL DEFAULT false (0001_init.sql) and has
// never had a writer — neither of the two production INSERTs in alerts.go names
// it. This repo has already fixed the same defect twice, in two READ surfaces:
// internal/api/server.go reported a structural 0 as a measured cache hit rate,
// and internal/mcp/server.go's get_cache_stats answered $0.00 of savings for a
// year. Migration 0100 added serve_source precisely so a cache serve is
// countable, and it IS written — insertCacheServeSQL binds it on every hit.
//
// The audit export is the THIRD read surface and it was left behind by both
// fixes: it selects `cached` beside `serve_source` and ships it to a SIEM.
//
// ⚠ WHY THIS IS NOT "A FALSE THAT IS AT LEAST HONESTLY FALSE" — the argument
// catalog/writerless_column_guard_test.go makes for excluding bare column-list
// mentions from its scan, and the reason that guard watched this happen. On an
// aggregate a constant column shows up as a suspicious zero. On a per-request
// COMPLIANCE EXPORT it is a per-row factual claim about a request that really
// happened: `"cached": false` on a response that was served from cache says the
// provider was called when it was not. Every row of every export, in all three
// formats, for every workspace, since 0001.
//
// ⚠ THE ROWS ARE WRITTEN BY THE PRODUCTION WRITERS, NOT BY A HAND-ROLLED
// INSERT. A test INSERT that names `cached` itself would prove the exporter can
// read a column — it would prove nothing about what production stores, which is
// the whole defect. alerts.RecordCacheServe / RecordSpend / RecordNodeServe are
// the three seams proxy.serve actually calls.
func serveSourceTestManager(t *testing.T, pool *pgxpool.Pool) *alerts.AlertManager {
	t.Helper()
	return alerts.New(pool, nil, nil)
}

// removeOwnRows deletes this test's own rows when it finishes.
//
// ⚠ IT IS NOT TIDINESS — IT IS THE DIFFERENCE BETWEEN A TEST AND A TRAP, AND
// THIS PACKAGE ALREADY KNEW: retention_integration_test.go#seedTokenEvent
// carries a comment explaining that a carelessly-seeded row "would make a seeded
// row poison a sibling export test". Two tests here read the WHOLE table —
// TestScheduledExport_AdvancesWatermarkOnSuccess sets the watermark to EPOCH and
// exports everything above it — so any row any earlier test leaves behind is
// inside their subject. Go orders test files by filename, and this file sorts
// BEFORE exporter_test.go, so without this cleanup the mere NAME of the file
// decides whether two unrelated tests pass. Measured, not feared: leaving the
// rows in place reds TestRetention_RequireExportOff_AgeOnly and
// TestScheduledExport_AdvancesWatermarkOnSuccess, and renaming this file to sort
// last turns both green again with no other change.
//
// token_events is append-only (migration 0055), and `SET LOCAL
// lens.audit_retention = 'on'` is the ONLY delete the trigger sanctions — the
// same scoped, transaction-local flag retention.go#deleteBatch uses, obtained
// from the migration rather than invented here.
func removeOwnRows(t *testing.T, pool *pgxpool.Pool, ws string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Errorf("cleanup begin: %v", err)
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL lens.audit_retention = 'on'`); err != nil {
			t.Errorf("cleanup set flag: %v", err)
			return
		}
		if _, err := tx.Exec(ctx, `DELETE FROM token_events WHERE workspace_id = $1`, ws); err != nil {
			t.Errorf("cleanup delete: %v", err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("cleanup commit: %v", err)
		}
	})
}

// exportedRows runs the real exporter over one workspace and returns the decoded
// NDJSON records keyed by request_id.
func exportedRows(t *testing.T, pool *pgxpool.Pool, ws string) map[string]AuditRecord {
	t.Helper()
	var buf bytes.Buffer
	n, err := New(pool).Export(context.Background(), ExportFilter{WorkspaceID: ws}, FormatNDJSON, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	out := make(map[string]AuditRecord, n)
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec AuditRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode NDJSON line %q: %v", line, err)
		}
		out[rec.RequestID] = rec
	}
	return out
}

// TestExport_CachedReflectsTheServeThatActuallyHappened drives all four serve
// kinds through the production writers and reads them back through the real
// exporter.
//
// ⚠ IT ASSERTS BOTH DIRECTIONS AND THE BOUNDARY. A rule that answered `true`
// for everything would be as wrong as the constant false it replaces, and the
// obvious wrong rule — `serve_source <> 'upstream'` — is caught by the NODE
// case: migration 0101 added 'node' to the enum, and a node serve is a MISS
// (the node did the compute; no cache produced the bytes). RecordNodeServe's
// own doc says so, and the hit-rate query migration 0100 documents is
// `serve_source LIKE 'cache_hit%'`, not `<> 'upstream'`.
func TestExport_CachedReflectsTheServeThatActuallyHappened(t *testing.T) {
	pool := auditTestPool(t)
	const ws = "ws_audit_serve_source"
	ctx := context.Background()
	am := serveSourceTestManager(t, pool)
	removeOwnRows(t, pool, ws)

	// Clean slate for this workspace: token_events is append-only under the 0055
	// triggers, so use a workspace nothing else touches and assert on request_id.
	const (
		reqUpstream = "req-ss-upstream"
		reqExact    = "req-ss-cache-exact"
		reqPooled   = "req-ss-cache-pooled-semantic"
		reqNode     = "req-ss-node"
	)

	if err := am.RecordSpend(ctx, ws, "t", "s", "f", "gpt-4o", 100, 50, "prompt", "sess", reqUpstream, "text", false); err != nil {
		t.Fatalf("RecordSpend (upstream): %v", err)
	}
	if err := am.RecordCacheServe(ctx, ws, "t", "s", "f", "gpt-4o", 100, 50, "sess", reqExact, "text", "cache_hit_exact"); err != nil {
		t.Fatalf("RecordCacheServe (exact): %v", err)
	}
	if err := am.RecordCacheServe(ctx, ws, "t", "s", "f", "gpt-4o", 100, 50, "sess", reqPooled, "text", "cache_hit_pooled_semantic"); err != nil {
		t.Fatalf("RecordCacheServe (pooled semantic): %v", err)
	}
	if err := am.RecordNodeServe(ctx, ws, "t", "s", "f", "gpt-4o", 100, 50, "sess", reqNode, "text"); err != nil {
		t.Fatalf("RecordNodeServe: %v", err)
	}

	rows := exportedRows(t, pool, ws)

	for _, tc := range []struct {
		reqID string
		want  bool
		why   string
	}{
		{reqExact, true, "an EXACT cache hit was served from cache — serve_source='cache_hit_exact'"},
		{reqPooled, true, "a POOLED SEMANTIC hit was served from cache — serve_source='cache_hit_pooled_semantic'"},
		{reqUpstream, false, "an upstream serve really did call the provider — serve_source='upstream'"},
		{reqNode, false, "a NODE serve is a cache MISS: the node did the compute, no cache produced the bytes (0101)"},
	} {
		rec, ok := rows[tc.reqID]
		if !ok {
			t.Fatalf("request %q missing from the export entirely (got %d rows: %v)", tc.reqID, len(rows), keysOf(rows))
		}
		if rec.Cached != tc.want {
			t.Errorf("exported cached = %v for %s, want %v — %s.\n"+
				"The export reads token_events.cached, which NOTHING WRITES (every row carries the "+
				"0001_init.sql default). serve_source is written on the SAME ROW and says what "+
				"actually served it. This is the third read surface to make this mistake; the first "+
				"two were api/server.go and mcp/server.go.",
				rec.Cached, tc.reqID, tc.want, tc.why)
		}
	}
}

// ⚠⚠ THE TWO PINS BELOW RECORD DEFECTS THAT ARE **NOT** FIXED. They assert the
// CONSTANT, so they go GREEN over broken behaviour on purpose — which is only
// defensible because the alternative is worse: an undocumented constant on a
// compliance export is indistinguishable from a measurement, and that is exactly
// how `cached` survived two fixes of its own defect. A pin makes the constant a
// reviewable fact with a name. Each one FAILS the day the field starts carrying
// real data, and the correct response to that failure is to DELETE the pin, not
// to edit the export.

// TestExport_PIIDetectedIsAConstant — pii_detected is exported from a column
// NOTHING WRITES, and unlike `cached` it cannot be derived: no column of
// token_events carries the answer.
//
// The product DOES know it. proxy.go computes piiDetected per request and hands
// it to the learner; the billing insert (alerts.go#insertTokenEventSQL) simply
// never names the column, so the value is dropped between detection and the row
// every compliance export reads.
//
// ⚠ THE TRIPWIRE IS THE CALL BELOW, NOT THE ASSERTION. RecordSpend has no PII
// parameter at all — that signature is the whole evidence for "writerless". If
// someone gives it one, THIS FILE STOPS COMPILING and the author is forced to
// look here. An assertion alone could not tell "no writer" from "a writer that
// happened to store false".
func TestExport_PIIDetectedIsAConstant(t *testing.T) {
	pool := auditTestPool(t)
	const ws = "ws_audit_pii_constant"
	ctx := context.Background()
	am := serveSourceTestManager(t, pool)
	removeOwnRows(t, pool, ws)

	const reqID = "req-pii-constant"
	if err := am.RecordSpend(ctx, ws, "t", "s", "f", "gpt-4o", 10, 5, "my SSN is 123-45-6789", "sess", reqID, "text", false); err != nil {
		t.Fatalf("RecordSpend: %v", err)
	}

	rec, ok := exportedRows(t, pool, ws)[reqID]
	if !ok {
		t.Fatalf("request %q missing from the export", reqID)
	}
	if rec.PIIDetected {
		t.Fatalf("pii_detected = true — token_events.pii_detected HAS GAINED A WRITER.\n" +
			"That is good news and this pin is now wrong: delete TestExport_PIIDetectedIsAConstant " +
			"and the paragraph in exporter.go#buildQuery that says the value cannot be derived.")
	}
	t.Logf("PINNED KNOWN DEFECT: pii_detected exported as %v for a prompt containing PII — "+
		"the column has no writer, so every audit row in every workspace has said 'false' since "+
		"0001_init.sql. Not a measurement.", rec.PIIDetected)
}

// TestExport_BranchIsAConstant — `branch` is promised by the CSV header and the
// JSON/NDJSON field, and NO QUERY FILLS IT. buildQuery does not select it,
// scanRow does not scan it, and token_events has no branch column at all;
// exporter.go's CSV row is the only read of AuditRecord.Branch in the repo.
//
// Branch attribution is real and lives elsewhere (migrations 0004_branch_spend,
// 0017_attribution), keyed by its own context rather than by token_events'
// request_id — so filling this field is a JOIN and a decision about row
// semantics, not a column swap like `cached` was.
func TestExport_BranchIsAConstant(t *testing.T) {
	pool := auditTestPool(t)
	const ws = "ws_audit_branch_constant"
	ctx := context.Background()
	am := serveSourceTestManager(t, pool)
	removeOwnRows(t, pool, ws)

	const reqID = "req-branch-constant"
	if err := am.RecordSpend(ctx, ws, "t", "s", "f", "gpt-4o", 10, 5, "p", "sess", reqID, "text", false); err != nil {
		t.Fatalf("RecordSpend: %v", err)
	}

	rec, ok := exportedRows(t, pool, ws)[reqID]
	if !ok {
		t.Fatalf("request %q missing from the export", reqID)
	}
	if rec.Branch != "" {
		t.Fatalf("branch = %q — the export has started carrying real branch attribution.\n"+
			"Delete TestExport_BranchIsAConstant; it pinned the empty string.", rec.Branch)
	}

	// And the CSV header keeps promising the column, which is the half a JSON-only
	// check would miss: a consumer parsing by header position gets a real column of
	// empty strings, not an absent field.
	var csvBuf bytes.Buffer
	if _, err := New(pool).Export(ctx, ExportFilter{WorkspaceID: ws}, FormatCSV, &csvBuf); err != nil {
		t.Fatalf("Export CSV: %v", err)
	}
	header := strings.Split(strings.SplitN(csvBuf.String(), "\n", 2)[0], ",")
	branchCol := -1
	for i, h := range header {
		if h == "branch" {
			branchCol = i
		}
	}
	if branchCol == -1 {
		t.Fatalf("csvHeader no longer promises a 'branch' column — delete this pin, the mismatch is gone")
	}
	t.Logf("PINNED KNOWN DEFECT: CSV column %d is named 'branch' and every row's value is the "+
		"empty string; NDJSON carries \"branch\":\"\". No query fills it.", branchCol)
}

func keysOf(m map[string]AuditRecord) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
