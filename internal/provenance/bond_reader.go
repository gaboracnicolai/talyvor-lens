package provenance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Bond is the read projection of a provenance bond (all provenance_bonds columns). Money is integer µLENS.
// appeal_deadline is surfaced so a UI can show when the slash finalizes (a bond view is useless without it).
type Bond struct {
	BondID         string     `json:"bond_id"`
	WorkspaceID    string     `json:"workspace_id"`
	OutputID       string     `json:"output_id"`
	AmountULens    int64      `json:"amount_ulens"`
	SlashBps       int        `json:"slash_bps"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	AppealDeadline time.Time  `json:"appeal_deadline"`
	SettledAt      *time.Time `json:"settled_at,omitempty"`
}

// bondReadDB is the query-only seam (the primary *pgxpool.Pool satisfies it). No Begin/Exec — a read-back
// holds no write path, and it is wired on the PRIMARY pool (bond reads are money-adjacent: collateral +
// appeal windows must not be read from a lagging replica).
type bondReadDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// BondReader is QUERY-ONLY.
type BondReader struct{ db bondReadDB }

func NewBondReader(db bondReadDB) *BondReader { return &BondReader{db: db} }

const bondSelectCols = `bond_id, workspace_id, output_id, amount_ulens, slash_bps, status, created_at, appeal_deadline, settled_at`

// ListByWorkspace returns the workspace's OWN bonds, newest-first (WHERE workspace_id = $1 — intra-tenant;
// there is no unscoped tenant read). Uses idx_provenance_bonds_ws (workspace_id, status).
const listBondsByWorkspaceSQL = `SELECT ` + bondSelectCols + `
FROM provenance_bonds WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT $2`

func (r *BondReader) ListByWorkspace(ctx context.Context, workspaceID string, limit int) ([]Bond, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.db.Query(ctx, listBondsByWorkspaceSQL, workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("provenance: list bonds: %w", err)
	}
	defer rows.Close()
	var out []Bond
	for rows.Next() {
		b, err := scanBond(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetByID is OWNER-SCOPED: WHERE bond_id = $1 AND workspace_id = $2. A bond_id belonging to another
// workspace resolves to (Bond{}, false, nil) — the handler renders that as 404 (never 403), so the surface
// gives no cross-tenant existence oracle.
const getBondByIDSQL = `SELECT ` + bondSelectCols + `
FROM provenance_bonds WHERE bond_id = $1 AND workspace_id = $2`

func (r *BondReader) GetByID(ctx context.Context, workspaceID, bondID string) (Bond, bool, error) {
	if r == nil || r.db == nil {
		return Bond{}, false, nil
	}
	b, err := scanBond(r.db.QueryRow(ctx, getBondByIDSQL, bondID, workspaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Bond{}, false, nil
	}
	if err != nil {
		return Bond{}, false, fmt.Errorf("provenance: get bond: %w", err)
	}
	return b, true, nil
}

// scannable is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query).
type scannable interface{ Scan(dest ...any) error }

func scanBond(s scannable) (Bond, error) {
	var b Bond
	if err := s.Scan(&b.BondID, &b.WorkspaceID, &b.OutputID, &b.AmountULens, &b.SlashBps,
		&b.Status, &b.CreatedAt, &b.AppealDeadline, &b.SettledAt); err != nil {
		return Bond{}, err
	}
	return b, nil
}
