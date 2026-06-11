// reader.go — the ADMIN-ONLY read surface over distill_serve_attribution (the
// S1 read-surface commitment). It is DELIBERATELY a separate type from Store:
//
// STRUCTURAL INERTNESS (preserved): Store is Exec-only (write, no read); Reader
// is Query-only (read, no write). Neither imports a ledger or exposes a credit
// method. A Reader cannot write a row, open a transaction, or mint — it only
// SELECTs. The route that exposes it is admin-gated (requireAdmin); content_hash
// and counterparty workspace ids must never be tenant-reachable.
package distillattrib

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// readDB is the minimal READ seam — Query only, no Exec/Begin (so a Reader is
// provably read-only). Matches the repo's pgxDB/marginDB convention; nil
// degrades to a no-op (empty result), like Store and MarginReader.
type readDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Reader is the admin-only read surface. Holds no write capability and no
// ledger reference — distinct from the Exec-only Store on purpose.
type Reader struct {
	db readDB
}

// NewReader builds a Reader over a Query-capable pool. nil → an inert reader
// (every method returns empty), so it is safe to wire before any data exists.
func NewReader(db readDB) *Reader { return &Reader{db: db} }

// ServeRow is one raw attribution row (admin-only — exposes both workspace ids
// and the content_hash).
type ServeRow struct {
	OwnerWorkspaceID     string    `json:"owner_workspace_id"`
	RequesterWorkspaceID string    `json:"requester_workspace_id"`
	ContentHash          string    `json:"content_hash"`
	ServeCount           int64     `json:"serve_count"`
	FirstServedAt        time.Time `json:"first_served_at"`
	LastServedAt         time.Time `json:"last_served_at"`
}

const rawRowsSQL = `SELECT owner_workspace_id, requester_workspace_id, content_hash,
       serve_count, first_served_at, last_served_at
FROM distill_serve_attribution
ORDER BY last_served_at DESC
LIMIT $1`

// RawRows returns raw attribution rows, most-recently-served first, capped at
// limit (the caller enforces the upper bound — see the handler's cap). ADMIN
// ONLY: content_hash + both workspace ids are returned verbatim.
func (r *Reader) RawRows(ctx context.Context, limit int) ([]ServeRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, rawRowsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("distillattrib: raw rows: %w", err)
	}
	defer rows.Close()
	var out []ServeRow
	for rows.Next() {
		var s ServeRow
		if err := rows.Scan(&s.OwnerWorkspaceID, &s.RequesterWorkspaceID, &s.ContentHash,
			&s.ServeCount, &s.FirstServedAt, &s.LastServedAt); err != nil {
			return nil, fmt.Errorf("distillattrib: scan raw row: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PairTotal is one (owner, requester) aggregate — the condition-(b) probe shape.
type PairTotal struct {
	OwnerWorkspaceID     string    `json:"owner_workspace_id"`
	RequesterWorkspaceID string    `json:"requester_workspace_id"`
	Serves               int64     `json:"serves"`
	LastServedAt         time.Time `json:"last_served_at"`
}

const pairTotalsSQL = `SELECT owner_workspace_id, requester_workspace_id,
       SUM(serve_count) AS serves, MAX(last_served_at) AS last_served
FROM distill_serve_attribution
GROUP BY owner_workspace_id, requester_workspace_id
ORDER BY serves DESC
LIMIT $1`

// PairTotals is the materiality probe: cross-tenant reuse per (owner, requester)
// pair, heaviest first, capped at limit. Feeds the parked royalty's
// condition-(b) decision ("is there material cross-tenant reuse in prod?").
// Served by the PK (owner, requester, content_hash) prefix — no new index.
func (r *Reader) PairTotals(ctx context.Context, limit int) ([]PairTotal, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, pairTotalsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("distillattrib: pair totals: %w", err)
	}
	defer rows.Close()
	var out []PairTotal
	for rows.Next() {
		var p PairTotal
		if err := rows.Scan(&p.OwnerWorkspaceID, &p.RequesterWorkspaceID, &p.Serves, &p.LastServedAt); err != nil {
			return nil, fmt.Errorf("distillattrib: scan pair total: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
