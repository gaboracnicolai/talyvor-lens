package costanomaly

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/budgets"
)

// pgxDB is the subset of *pgxpool.Pool the analytics reads need. Tests
// substitute pgxmock; a nil pool short-circuits reads to empty.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the read-only analytics source for cross-sectional cost
// anomalies. Issue units come from request_attribution (the only place
// issue_id + cost live); team/sprint units come from token_events.
type Store struct {
	db pgxDB
}

// NewStore builds a Store over the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &Store{db: db}
}

// newStore is the test seam (inject a pgxmock pool).
func newStore(db pgxDB) *Store { return &Store{db: db} }

// issueCostsSQL sums realized cost per issue from the attribution detail
// table. Only issues with positive spend over the window are returned, so
// the baseline is a distribution of real costs (median is always > 0).
const issueCostsSQL = `SELECT issue_id, SUM(cost_usd)
FROM request_attribution
WHERE workspace_id = $1
  AND issue_id <> ''
  AND created_at >= $2
GROUP BY issue_id
HAVING SUM(cost_usd) > 0`

// UnitCosts returns per-unit summed cost since `since`. unitKind is one of
// "issue", "team", "sprint". Team/sprint read token_events using the shared
// budgets.ScopeColumn mapping (team → "team", sprint → "sprint_id") so the
// scope→column rule isn't duplicated.
func (s *Store) UnitCosts(ctx context.Context, ws, unitKind string, since time.Time) ([]UnitCost, error) {
	if s.db == nil {
		return nil, nil
	}
	var sql string
	switch unitKind {
	case UnitIssue:
		sql = issueCostsSQL
	case UnitTeam, UnitSprint:
		col := budgets.ScopeColumn(budgets.Scope(unitKind))
		if col == "" {
			return nil, fmt.Errorf("costanomaly: no token_events column for unit %q", unitKind)
		}
		sql = fmt.Sprintf(`SELECT %s, SUM(cost_usd)
FROM token_events
WHERE workspace_id = $1
  AND %s <> ''
  AND created_at >= $2
GROUP BY %s
HAVING SUM(cost_usd) > 0`, col, col, col)
	default:
		return nil, fmt.Errorf("costanomaly: unknown unit kind %q", unitKind)
	}

	rows, err := s.db.Query(ctx, sql, ws, since)
	if err != nil {
		return nil, fmt.Errorf("costanomaly: unit costs (%s): %w", unitKind, err)
	}
	defer rows.Close()
	var out []UnitCost
	for rows.Next() {
		var uc UnitCost
		if err := rows.Scan(&uc.UnitID, &uc.CostUSD); err != nil {
			return nil, fmt.Errorf("costanomaly: scan unit cost: %w", err)
		}
		out = append(out, uc)
	}
	return out, rows.Err()
}
