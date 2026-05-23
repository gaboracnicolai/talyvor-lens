package audit

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func auditRows(rows ...AuditRecord) *pgxmock.Rows {
	r := pgxmock.NewRows([]string{
		"created_at", "workspace_id", "team", "feature",
		"provider", "model", "input_tokens", "output_tokens",
		"cost_usd", "cached", "pii_detected", "session_id", "request_id",
	})
	for _, rec := range rows {
		r.AddRow(
			rec.Timestamp, rec.WorkspaceID, rec.Team, rec.Feature,
			rec.Provider, rec.Model, rec.InputTokens, rec.OutputTokens,
			rec.CostUSD, rec.Cached, rec.PIIDetected, rec.SessionID, rec.RequestID,
		)
	}
	return r
}

func TestExport_JSON_ReturnsValidJSONArray(t *testing.T) {
	pool := newPool(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM token_events`).WithArgs(10000).WillReturnRows(auditRows(
		AuditRecord{Timestamp: now, WorkspaceID: "ws-1", Provider: "openai", Model: "gpt-4", InputTokens: 100, OutputTokens: 50, CostUSD: 0.012, RequestID: "req-1"},
		AuditRecord{Timestamp: now, WorkspaceID: "ws-1", Provider: "anthropic", Model: "claude-sonnet-4-6", InputTokens: 200, OutputTokens: 80, CostUSD: 0.005, RequestID: "req-2"},
	))

	e := newExporter(pool)
	var buf bytes.Buffer
	count, err := e.Export(context.Background(), ExportFilter{}, FormatJSON, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	var parsed []AuditRecord
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("output is not a JSON array: %v\nbody: %s", err, buf.String())
	}
	if len(parsed) != 2 {
		t.Errorf("parsed array len = %d, want 2", len(parsed))
	}
	if parsed[0].Provider != "openai" || parsed[1].Provider != "anthropic" {
		t.Errorf("provider order wrong: %+v", parsed)
	}
}

func TestExport_CSV_HeaderRowPlusDataRows(t *testing.T) {
	pool := newPool(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM token_events`).WithArgs(10000).WillReturnRows(auditRows(
		AuditRecord{Timestamp: now, WorkspaceID: "ws-1", Team: "billing,team", Provider: "openai", Model: "gpt-4", InputTokens: 100, OutputTokens: 50, CostUSD: 0.012},
	))

	e := newExporter(pool)
	var buf bytes.Buffer
	count, err := e.Export(context.Background(), ExportFilter{}, FormatCSV, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}

	rdr := csv.NewReader(&buf)
	records, err := rdr.ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d csv rows including header, want 2", len(records))
	}
	header := records[0]
	wantHeader := []string{"timestamp", "workspace_id", "team", "feature", "provider", "model", "input_tokens", "output_tokens", "cost_usd", "cached", "pii_detected", "branch", "session_id", "request_id"}
	if len(header) != len(wantHeader) {
		t.Errorf("header len = %d, want %d", len(header), len(wantHeader))
	}
	for i, h := range wantHeader {
		if i < len(header) && header[i] != h {
			t.Errorf("header[%d] = %q, want %q", i, header[i], h)
		}
	}
	// The team field containing a comma must round-trip correctly (proper
	// CSV quoting). encoding/csv handles this for us; the test guards
	// against a regression that swaps the codec for naive Sprintf.
	teamCol := 2
	if records[1][teamCol] != "billing,team" {
		t.Errorf("team field = %q, want %q (CSV quoting broken)", records[1][teamCol], "billing,team")
	}
}

func TestExport_NDJSON_OneObjectPerLine(t *testing.T) {
	pool := newPool(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM token_events`).WithArgs(10000).WillReturnRows(auditRows(
		AuditRecord{Timestamp: now, Provider: "openai", Model: "gpt-4"},
		AuditRecord{Timestamp: now, Provider: "anthropic", Model: "sonnet"},
		AuditRecord{Timestamp: now, Provider: "google", Model: "gemini"},
	))

	e := newExporter(pool)
	var buf bytes.Buffer
	count, err := e.Export(context.Background(), ExportFilter{}, FormatNDJSON, &buf)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d ndjson lines, want 3; body:\n%s", len(lines), buf.String())
	}
	for i, line := range lines {
		var got AuditRecord
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Errorf("line %d not valid JSON object: %v", i, err)
		}
	}
}

func TestExport_FilterByWorkspaceIDAppliesWhereClause(t *testing.T) {
	pool := newPool(t)
	pool.ExpectQuery(`WHERE.*workspace_id = \$1`).
		WithArgs("ws-special", 10000).
		WillReturnRows(auditRows())

	e := newExporter(pool)
	var buf bytes.Buffer
	if _, err := e.Export(context.Background(), ExportFilter{WorkspaceID: "ws-special"}, FormatJSON, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("workspace_id filter not applied: %v", err)
	}
}

func TestExport_FilterByDateRangeAppliesWhereClause(t *testing.T) {
	pool := newPool(t)
	start := time.Now().UTC().Add(-24 * time.Hour)
	end := time.Now().UTC()
	pool.ExpectQuery(`WHERE.*created_at >= \$1.*created_at <= \$2`).
		WithArgs(start, end, 10000).
		WillReturnRows(auditRows())

	e := newExporter(pool)
	var buf bytes.Buffer
	if _, err := e.Export(context.Background(), ExportFilter{StartTime: start, EndTime: end}, FormatJSON, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("date filter not applied: %v", err)
	}
}

func TestExport_MaxRecordsHonoured(t *testing.T) {
	pool := newPool(t)
	pool.ExpectQuery(`LIMIT \$1`).WithArgs(7).WillReturnRows(auditRows())

	e := newExporter(pool)
	var buf bytes.Buffer
	if _, err := e.Export(context.Background(), ExportFilter{MaxRecords: 7}, FormatJSON, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("MaxRecords limit not bound to LIMIT: %v", err)
	}

	// Hard ceiling: > 100000 must clamp.
	pool2 := newPool(t)
	pool2.ExpectQuery(`LIMIT \$1`).WithArgs(100000).WillReturnRows(auditRows())
	e2 := newExporter(pool2)
	buf.Reset()
	if _, err := e2.Export(context.Background(), ExportFilter{MaxRecords: 1_000_000}, FormatJSON, &buf); err != nil {
		t.Fatalf("Export huge: %v", err)
	}
	if err := pool2.ExpectationsWereMet(); err != nil {
		t.Errorf("MaxRecords did not clamp to 100000: %v", err)
	}
}

func TestExportWebhook_POSTsNDJSONWithExpectedHeaders(t *testing.T) {
	pool := newPool(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM token_events`).WithArgs(10000).WillReturnRows(auditRows(
		AuditRecord{Timestamp: now, Provider: "openai", Model: "gpt-4", InputTokens: 10, OutputTokens: 5},
		AuditRecord{Timestamp: now, Provider: "anthropic", Model: "sonnet", InputTokens: 20, OutputTokens: 10},
	))

	var gotCT, gotExport, gotCount string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotExport = r.Header.Get("X-Talyvor-Export")
		gotCount = r.Header.Get("X-Talyvor-Record-Count")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	e := newExporter(pool)
	if err := e.ExportWebhook(context.Background(), srv.URL, ExportFilter{}); err != nil {
		t.Fatalf("ExportWebhook: %v", err)
	}
	if gotExport != "true" {
		t.Errorf("X-Talyvor-Export = %q, want true", gotExport)
	}
	if gotCount != "2" {
		t.Errorf("X-Talyvor-Record-Count = %q, want 2", gotCount)
	}
	if !strings.HasPrefix(gotCT, "application/x-ndjson") {
		t.Errorf("Content-Type = %q, want application/x-ndjson", gotCT)
	}
	if !strings.Contains(string(gotBody), `"provider":"openai"`) || !strings.Contains(string(gotBody), `"provider":"anthropic"`) {
		t.Errorf("webhook body missing one or both records:\n%s", gotBody)
	}
}

func TestExport_EmptyResultReturnsEmptyExportNotError(t *testing.T) {
	pool := newPool(t)
	pool.ExpectQuery(`FROM token_events`).WithArgs(10000).WillReturnRows(auditRows())

	e := newExporter(pool)
	var buf bytes.Buffer
	count, err := e.Export(context.Background(), ExportFilter{}, FormatJSON, &buf)
	_ = count
	if err != nil {
		t.Errorf("empty result should not error; got %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
	// JSON form: must still be parseable as an array (empty array, not blank).
	var parsed []AuditRecord
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Errorf("empty JSON export not valid JSON: %v; body=%q", err, buf.String())
	}
}
