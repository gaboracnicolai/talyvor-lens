// findings_writer.go — the append-only sink for DetectorSweep findings. The ONLY write
// surface in the sweep path: Record runs a single INSERT … ON CONFLICT (identity_key) DO
// NOTHING into royalty_detector_findings — no UPDATE, no DELETE, no other table. The
// findingsDB seam is Exec-only (no Begin/Query → no transaction, no read of the money
// tables, no held-burn/credit primitive reachable).
package poolroyalty

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/dbjson"
)

// findingsDB is the narrow write seam — Exec ONLY (no Begin/Query). *pgxpool.Pool satisfies it.
type findingsDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// FindingsWriter records detector findings, append-only. The zero/nil writer is inert.
type FindingsWriter struct {
	db findingsDB
}

// NewFindingsWriter builds the append-only findings sink over db (the main pool).
func NewFindingsWriter(db findingsDB) *FindingsWriter { return &FindingsWriter{db: db} }

const insertFindingSQL = `INSERT INTO royalty_detector_findings
    (economy, detector, identity_key, contributor_workspace_id, requester_workspace_id, entry_or_content, window_seconds, metrics)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (identity_key) DO NOTHING`

// Record inserts ONE finding; returns inserted=false on a dedup conflict (already recorded
// — the append-only guarantee: an existing row is NEVER updated). dbjson.Marshal so metrics
// encodes as JSON text on both pgx protocols (#133); nilIfEmpty maps "" → NULL.
func (w *FindingsWriter) Record(ctx context.Context, f Finding) (bool, error) {
	if w == nil || w.db == nil {
		return false, nil
	}
	metricsJSON, err := dbjson.Marshal(f.Metrics)
	if err != nil {
		return false, err
	}
	tag, err := w.db.Exec(ctx, insertFindingSQL,
		f.Economy, f.Detector, f.IdentityKey, f.ContributorWorkspace,
		nilIfEmpty(f.RequesterWorkspace), nilIfEmpty(f.EntryOrContent), f.WindowSeconds, metricsJSON)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
