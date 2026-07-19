package outputverify

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// writeDB is an EXEC-ONLY seam (no Begin/Query) — the Writer is structurally incapable of anything but the
// single append-only INSERT. readDB is a QUERY-ONLY seam. Mirrors keel's never-acts discipline.
type writeDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
type readDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// VerdictRecord is one per-output intrinsic verdict to persist. Hashes only — never raw prompt/response text.
type VerdictRecord struct {
	OutputID       string
	WorkspaceID    string // SELF only
	Model          string
	Verdict        string // passed | failed_constraint | unverifiable
	Reason         string
	ConstraintKind string
	PromptSHA256   string
	ResponseSHA256 string
	// OutputContentSHA256 is the H5 content binding: hex(sha256(canonical assistant text)) per
	// CanonicalContentSHA256 (content.go pins the byte definition). "" = no committable content
	// (stored as NULL) — the output can never opt in to an artifact commitment.
	OutputContentSHA256 string
}

// Writer is the ONLY write path — an append-only sink for k4_output_verdicts. It holds a writeDB and runs
// exactly one INSERT constant; it NEVER touches any economy/ledger/held table (structurally + import-guard).
type Writer struct{ db writeDB }

func NewWriter(db writeDB) *Writer { return &Writer{db: db} }

const insertVerdictSQL = `INSERT INTO k4_output_verdicts
    (output_id, workspace_id, model, verdict, reason, constraint_kind, prompt_sha256, response_sha256, output_content_sha256)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''))
ON CONFLICT (output_id) DO NOTHING`

// Record appends one verdict; returns inserted=false on a dedup conflict (a re-served identical output, or a
// replay). Never updates an existing row.
func (w *Writer) Record(ctx context.Context, r VerdictRecord) (bool, error) {
	if w == nil || w.db == nil {
		return false, nil
	}
	tag, err := w.db.Exec(ctx, insertVerdictSQL,
		r.OutputID, r.WorkspaceID, r.Model, r.Verdict, r.Reason, r.ConstraintKind, r.PromptSHA256, r.ResponseSHA256, r.OutputContentSHA256)
	if err != nil {
		return false, fmt.Errorf("outputverify: record verdict: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListedVerdict is the read projection (hashes only; no raw content exists to read).
type ListedVerdict struct {
	OutputID       string    `json:"output_id"`
	WorkspaceID    string    `json:"workspace_id"`
	Model          string    `json:"model"`
	Verdict        string    `json:"verdict"`
	Reason         string    `json:"reason"`
	ConstraintKind string    `json:"constraint_kind"`
	CreatedAt      time.Time `json:"created_at"`
}

// Reader is QUERY-ONLY.
type Reader struct{ db readDB }

func NewReader(db readDB) *Reader { return &Reader{db: db} }

const selectCols = `output_id, workspace_id, model, verdict, reason, constraint_kind, created_at`

// ListForWorkspace returns ONLY the given workspace's own verdicts (intra-tenant: WHERE workspace_id = $1).
// A tenant may read its own; it can never name another's (the filter is not optional — there is no unscoped
// tenant read).
const listForWorkspaceSQL = `SELECT ` + selectCols + `
FROM k4_output_verdicts WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT $2`

func (r *Reader) ListForWorkspace(ctx context.Context, workspaceID string, limit int) ([]ListedVerdict, error) {
	return r.query(ctx, listForWorkspaceSQL, workspaceID, clampLimit(limit))
}

// ListAll returns verdicts across workspaces — the requireAdmin forensic read (gated at the mount site).
const listAllSQL = `SELECT ` + selectCols + `
FROM k4_output_verdicts ORDER BY created_at DESC LIMIT $1`

func (r *Reader) ListAll(ctx context.Context, limit int) ([]ListedVerdict, error) {
	return r.query(ctx, listAllSQL, clampLimit(limit))
}

func clampLimit(limit int) int {
	if limit <= 0 || limit > 1000 {
		return 100
	}
	return limit
}

func (r *Reader) query(ctx context.Context, sql string, args ...any) ([]ListedVerdict, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("outputverify: list verdicts: %w", err)
	}
	defer rows.Close()
	var out []ListedVerdict
	for rows.Next() {
		var v ListedVerdict
		if err := rows.Scan(&v.OutputID, &v.WorkspaceID, &v.Model, &v.Verdict, &v.Reason, &v.ConstraintKind, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("outputverify: scan verdict: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
