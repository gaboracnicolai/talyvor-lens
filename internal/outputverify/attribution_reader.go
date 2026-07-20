package outputverify

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AttributionRecord is the read projection of an output_attributions row (with created_at, which the write
// struct Attribution omits). target_ref is an opaque string — never parsed or dereferenced.
type AttributionRecord struct {
	OutputID    string    `json:"output_id"`
	WorkspaceID string    `json:"workspace_id"`
	TargetKind  string    `json:"target_kind"`
	TargetRef   string    `json:"target_ref"`
	CreatedAt   time.Time `json:"created_at"`
}

// attributionReadDB is the query-only seam (the primary *pgxpool.Pool satisfies it). Reads run on the
// primary, consistent with the attribution writer (never a replica).
type attributionReadDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// AttributionReader is QUERY-ONLY — the read-back for output_attributions.
type AttributionReader struct{ db attributionReadDB }

func NewAttributionReader(db attributionReadDB) *AttributionReader { return &AttributionReader{db: db} }

const attrSelectCols = `output_id, workspace_id, target_kind, target_ref, created_at`

// GetByOutput returns the attributions of ONE output that the workspace OWNS (WHERE output_id = $1 AND
// workspace_id = $2). A single output can carry a pr AND a spec (PK = output_id, workspace_id, target_kind),
// so this returns a slice. A foreign output → zero rows → the handler renders 404 (no cross-tenant oracle).
const getAttributionByOutputSQL = `SELECT ` + attrSelectCols + `
FROM output_attributions WHERE output_id = $1 AND workspace_id = $2 ORDER BY target_kind`

func (r *AttributionReader) GetByOutput(ctx context.Context, workspaceID, outputID string) ([]AttributionRecord, error) {
	return r.query(ctx, getAttributionByOutputSQL, outputID, workspaceID)
}

// ListByWorkspace returns the workspace's OWN attributions, newest-first (WHERE workspace_id = $1).
const listAttributionsByWorkspaceSQL = `SELECT ` + attrSelectCols + `
FROM output_attributions WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT $2`

func (r *AttributionReader) ListByWorkspace(ctx context.Context, workspaceID string, limit int) ([]AttributionRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return r.query(ctx, listAttributionsByWorkspaceSQL, workspaceID, limit)
}

func (r *AttributionReader) query(ctx context.Context, sql string, args ...any) ([]AttributionRecord, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("outputverify: list attributions: %w", err)
	}
	defer rows.Close()
	var out []AttributionRecord
	for rows.Next() {
		var a AttributionRecord
		if err := rows.Scan(&a.OutputID, &a.WorkspaceID, &a.TargetKind, &a.TargetRef, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("outputverify: scan attribution: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
