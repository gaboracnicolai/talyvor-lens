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
// UnitCosts returns per-unit summed cost since `since`, with no upper time
// bound (i.e. up to now). Thin wrapper over UnitCostsWindow — the detector
// calls this.
func (s *Store) UnitCosts(ctx context.Context, ws, unitKind string, since time.Time) ([]UnitCost, error) {
	return s.UnitCostsWindow(ctx, ws, unitKind, since, time.Time{})
}

// UnitCostsWindow returns per-unit summed cost over [since, until). A zero
// `until` means "no upper bound" (up to now), preserving UnitCosts'
// behaviour exactly (same SQL, same args). The bounded form exists so the
// ROI report can take period-over-period and monthly-trend windows without
// duplicating this scope→column aggregation.
//
// unitKind is "issue" (request_attribution / issue_id) or "team"/"sprint"
// (token_events, via the shared budgets.ScopeColumn mapping). Only units
// with positive spend are returned, so a baseline median is always > 0.
func (s *Store) UnitCostsWindow(ctx context.Context, ws, unitKind string, since, until time.Time) ([]UnitCost, error) {
	if s.db == nil {
		return nil, nil
	}
	// allowedQueries is the closed whitelist of permitted (table, column) pairs.
	// Column and table names are structural SQL identifiers — they cannot be
	// parameterised with $N placeholders — so we use a fixed lookup instead of
	// user-supplied strings to prevent any SQL injection path, even indirect.
	type tableCol struct{ table, col string }
	allowedQueries := map[string]tableCol{
		UnitIssue:  {"request_attribution", "issue_id"},
		UnitTeam:   {"token_events", budgets.ScopeColumn(budgets.ScopeTeam)},
		UnitSprint: {"token_events", budgets.ScopeColumn(budgets.ScopeSprint)},
	}
	tc, ok := allowedQueries[unitKind]
	if !ok || tc.col == "" {
		return nil, fmt.Errorf("costanomaly: unknown unit kind %q", unitKind)
	}
	table, col := tc.table, tc.col

	args := []any{ws, since}
	filterUntil := ""
	if !until.IsZero() {
		filterUntil = " AND created_at < $3"
		args = append(args, until)
	}
	// table and col are validated above against a closed whitelist —
	// no user input reaches this Sprintf.
	sql := fmt.Sprintf(
		"SELECT %s, SUM(cost_usd) FROM %s WHERE workspace_id = $1 AND %s <> '' AND created_at >= $2%s GROUP BY %s HAVING SUM(cost_usd) > 0",
		col, table, col, filterUntil, col,
	)

	rows, err := s.db.Query(ctx, sql, args...)
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
