package keel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// readDB is a QUERY-ONLY seam — no Exec/Begin is reachable through it, so the Reader is structurally
// incapable of mutating anything. (Mirrors poolroyalty's DetectorReader never-acts discipline.)
type readDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// writeDB is an EXEC-ONLY seam used solely by FindingsWriter for the single append-only INSERT below.
type writeDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Reader reads the CONSENTED cross-tenant corpus and returns per-(unit, workspace, window) AGGREGATES.
// It filters opted_in = TRUE (the dual opt-out reused verbatim: a non-opted-in workspace never has a
// routing_patterns row, and this read filter is belt-and-suspenders). A raw per-tenant output_quality
// value never leaves this query un-aggregated — AVG collapses each workspace's rows to one mean.
type Reader struct{ db readDB }

func NewReader(db readDB) *Reader { return &Reader{db: db} }

// cohortObservationsSQL: comparison unit = (provider_used, model_used); window = created_at bucketed by
// windowSeconds. ORDER BY makes the row stream deterministic (feeds the ordered reduction in Detect).
const cohortObservationsSQL = `
SELECT provider_used, model_used, workspace_id,
       floor(extract(epoch FROM created_at) / $1)::bigint AS window_bucket,
       AVG(output_quality) AS mean_quality,
       COUNT(*)            AS sample
FROM routing_patterns
WHERE opted_in = TRUE AND created_at >= $2
GROUP BY provider_used, model_used, workspace_id, window_bucket
ORDER BY provider_used, model_used, window_bucket, workspace_id`

// CohortObservations returns the aggregated corpus since `since`, bucketed into windowSeconds windows.
func (r *Reader) CohortObservations(ctx context.Context, windowSeconds int64, since time.Time) ([]Observation, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if windowSeconds <= 0 {
		windowSeconds = 3600
	}
	rows, err := r.db.Query(ctx, cohortObservationsSQL, windowSeconds, since)
	if err != nil {
		return nil, fmt.Errorf("keel: cohort observations: %w", err)
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var provider, model, ws string
		var window int64
		var mean float64
		var sample int
		if err := rows.Scan(&provider, &model, &ws, &window, &mean, &sample); err != nil {
			return nil, fmt.Errorf("keel: scan observation: %w", err)
		}
		out = append(out, Observation{
			Unit:        provider + "/" + model,
			WorkspaceID: ws,
			Window:      window,
			MeanQuality: mean,
			Sample:      sample,
		})
	}
	return out, rows.Err()
}

// ListFindings reads recorded findings newest-first (the requireAdmin read surface). Query-only. Every row
// names only a SELF workspace + aggregates, so an admin forensic read exposes no counterparty raw value.
const listFindingsSQL = `
SELECT workspace_id, unit, window_bucket, deviation_sigma, attribution, mode, cohort_n, first_seen_at
FROM keel_findings
ORDER BY first_seen_at DESC
LIMIT $1`

// listFindingsForWorkspaceSQL is the TENANT read: ONLY the caller's own rows (WHERE workspace_id = $1), hitting
// the (workspace_id, first_seen_at DESC) index from migration 0080. Same projection as the admin read.
const listFindingsForWorkspaceSQL = `
SELECT workspace_id, unit, window_bucket, deviation_sigma, attribution, mode, cohort_n, first_seen_at
FROM keel_findings
WHERE workspace_id = $1
ORDER BY first_seen_at DESC
LIMIT $2`

// ListedFinding is the read projection (admin + tenant). mode ('ordinary'|'hardened') and attribution
// ('idiosyncratic'|'common_mode') are both surfaced so a reader can tell a money-grade hardened finding from
// an ordinary one, and an idiosyncratic drift from a shared regression.
type ListedFinding struct {
	WorkspaceID    string    `json:"workspace_id"`
	Unit           string    `json:"unit"`
	Window         int64     `json:"window"`
	DeviationSigma float64   `json:"deviation_sigma"`
	Attribution    string    `json:"attribution"`
	Mode           string    `json:"mode"`
	CohortN        int       `json:"cohort_n"`
	FirstSeenAt    time.Time `json:"first_seen_at"`
}

func (r *Reader) ListFindings(ctx context.Context, limit int) ([]ListedFinding, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.db.Query(ctx, listFindingsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("keel: list findings: %w", err)
	}
	defer rows.Close()
	var out []ListedFinding
	for rows.Next() {
		var f ListedFinding
		if err := rows.Scan(&f.WorkspaceID, &f.Unit, &f.Window, &f.DeviationSigma, &f.Attribution, &f.Mode, &f.CohortN, &f.FirstSeenAt); err != nil {
			return nil, fmt.Errorf("keel: scan finding: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListFindingsForWorkspace reads ONLY workspaceID's own findings, newest-first. The scope is the caller's
// authenticated workspace — never a request param. Query-only.
func (r *Reader) ListFindingsForWorkspace(ctx context.Context, workspaceID string, limit int) ([]ListedFinding, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.db.Query(ctx, listFindingsForWorkspaceSQL, workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("keel: list findings for workspace: %w", err)
	}
	defer rows.Close()
	var out []ListedFinding
	for rows.Next() {
		var f ListedFinding
		if err := rows.Scan(&f.WorkspaceID, &f.Unit, &f.Window, &f.DeviationSigma, &f.Attribution, &f.Mode, &f.CohortN, &f.FirstSeenAt); err != nil {
			return nil, fmt.Errorf("keel: scan finding: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FindingsWriter is the ONLY write path in keel — an append-only sink for keel_findings. It NEVER touches
// any economy/ledger/held table (structurally: it holds a writeDB and runs exactly one INSERT constant).
type FindingsWriter struct{ db writeDB }

func NewFindingsWriter(db writeDB) *FindingsWriter { return &FindingsWriter{db: db} }

const insertFindingSQL = `INSERT INTO keel_findings
    (workspace_id, unit, window_bucket, deviation_sigma, attribution, cohort_n, identity_key, metrics)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (identity_key) DO NOTHING`

// IdentityKey dedups a finding across sweeps: sha256(keel:<workspace>:<unit>:<window>). One flag per
// (workspace, unit, window) — a re-sweep of the same window is a no-op.
func IdentityKey(f Finding) string {
	sum := sha256.Sum256([]byte("keel:" + f.WorkspaceID + ":" + f.Unit + ":" + strconv.FormatInt(f.Window, 10)))
	return hex.EncodeToString(sum[:])
}

// Record inserts ONE finding append-only; returns inserted=false on a dedup conflict (never updates an
// existing row). metrics carries the numeric evidence (cohort_mean/stddev/residual_shift) as JSON.
func (w *FindingsWriter) Record(ctx context.Context, f Finding, metrics map[string]any) (bool, error) {
	if w == nil || w.db == nil {
		return false, nil
	}
	mj, err := json.Marshal(metrics)
	if err != nil {
		return false, fmt.Errorf("keel: marshal metrics: %w", err)
	}
	tag, err := w.db.Exec(ctx, insertFindingSQL,
		f.WorkspaceID, f.Unit, f.Window, f.DeviationSigma, f.Attribution, f.CohortN, IdentityKey(f), string(mj))
	if err != nil {
		return false, fmt.Errorf("keel: record finding: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// insertHardenedFindingSQL writes a money-grade hardened finding with mode='hardened'. Same append-only
// discipline (ON CONFLICT DO NOTHING); the mode column (migration 0081) is what H5 filters on.
const insertHardenedFindingSQL = `INSERT INTO keel_findings
    (workspace_id, unit, window_bucket, deviation_sigma, attribution, cohort_n, identity_key, metrics, mode)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'hardened')
ON CONFLICT (identity_key) DO NOTHING`

// HardenedIdentityKey dedups a hardened finding across sweeps and — via the distinct "keelh:" prefix —
// yields a DIFFERENT digest from the ordinary IdentityKey for the same (workspace, unit, window), so an
// ordinary and a hardened finding for the same key never collide on the UNIQUE(identity_key) constraint.
func HardenedIdentityKey(f Finding) string {
	sum := sha256.Sum256([]byte("keelh:" + f.WorkspaceID + ":" + f.Unit + ":" + strconv.FormatInt(f.Window, 10)))
	return hex.EncodeToString(sum[:])
}

// RecordHardened inserts ONE hardened finding append-only (mode='hardened'); returns inserted=false on a
// dedup conflict. Mirrors Record but tags the row hardened and keys it with HardenedIdentityKey.
func (w *FindingsWriter) RecordHardened(ctx context.Context, f Finding, metrics map[string]any) (bool, error) {
	if w == nil || w.db == nil {
		return false, nil
	}
	mj, err := json.Marshal(metrics)
	if err != nil {
		return false, fmt.Errorf("keel: marshal hardened metrics: %w", err)
	}
	tag, err := w.db.Exec(ctx, insertHardenedFindingSQL,
		f.WorkspaceID, f.Unit, f.Window, f.DeviationSigma, f.Attribution, f.CohortN, HardenedIdentityKey(f), string(mj))
	if err != nil {
		return false, fmt.Errorf("keel: record hardened finding: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// listHardenedFindingsSQL is the hardened-only read (the SQL-enforceable filter H5 uses). It can NEVER
// return an ordinary finding.
const listHardenedFindingsSQL = `
SELECT workspace_id, unit, window_bucket, deviation_sigma, attribution, cohort_n, first_seen_at
FROM keel_findings
WHERE mode = 'hardened'
ORDER BY first_seen_at DESC
LIMIT $1`

// ListHardenedFindings returns ONLY hardened findings (mode='hardened'), newest-first. Query-only.
func (r *Reader) ListHardenedFindings(ctx context.Context, limit int) ([]ListedFinding, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.db.Query(ctx, listHardenedFindingsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("keel: list hardened findings: %w", err)
	}
	defer rows.Close()
	var out []ListedFinding
	for rows.Next() {
		var f ListedFinding
		if err := rows.Scan(&f.WorkspaceID, &f.Unit, &f.Window, &f.DeviationSigma, &f.Attribution, &f.CohortN, &f.FirstSeenAt); err != nil {
			return nil, fmt.Errorf("keel: scan hardened finding: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
