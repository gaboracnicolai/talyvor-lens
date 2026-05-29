package forecast

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/budgets"
)

// pgxDB is the subset of *pgxpool.Pool the daily-bucket query needs. Tests
// substitute pgxmock; a nil pool short-circuits reads to empty.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// budgetSpend is the slice of budgets.Store the forecaster reuses for
// period spend (ReconcileSpent) and limits (List). *budgets.Store satisfies
// it; reusing it keeps scope/period logic in one place.
type budgetSpend interface {
	ReconcileSpent(ctx context.Context, b budgets.Budget) (float64, error)
	List(ctx context.Context, workspaceID string) ([]budgets.Budget, error)
}

// Store is the read-only analytics data source for forecasting. It owns the
// per-day bucket query and delegates period spend + limits to budgets so
// the scope/period rules aren't duplicated.
type Store struct {
	db      pgxDB
	budgets budgetSpend
}

// NewStore builds a Store over the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	// Guard the typed-nil interface trap.
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &Store{db: db, budgets: budgets.NewStore(pool)}
}

// newStore is the test seam (inject a pgxmock pool and/or a budgets stub).
func newStore(db pgxDB, b budgetSpend) *Store { return &Store{db: db, budgets: b} }

// PeriodSpent delegates to budgets.ReconcileSpent — the single source of
// truth for scope+period spend.
func (s *Store) PeriodSpent(ctx context.Context, b budgets.Budget) (float64, error) {
	if s.budgets == nil {
		return 0, nil
	}
	return s.budgets.ReconcileSpent(ctx, b)
}

// Budgets delegates to budgets.List.
func (s *Store) Budgets(ctx context.Context, ws string) ([]budgets.Budget, error) {
	if s.budgets == nil {
		return nil, nil
	}
	return s.budgets.List(ctx, ws)
}

// DailyBuckets returns per-day summed spend for the scope over the last
// sinceDays days. Scope→column mapping is reused from budgets.ScopeColumn so
// reconciliation and forecasting agree. Days with no spend are simply absent
// from the result.
func (s *Store) DailyBuckets(ctx context.Context, ws string, scope budgets.Scope, scopeID string, sinceDays int, now time.Time) ([]DayBucket, error) {
	if s.db == nil {
		return nil, nil
	}
	where := "workspace_id = $1"
	args := []any{ws}
	if col := budgets.ScopeColumn(scope); col != "" {
		where += fmt.Sprintf(" AND %s = $%d", col, len(args)+1)
		args = append(args, scopeID)
	}
	since := now.AddDate(0, 0, -sinceDays)
	where += fmt.Sprintf(" AND created_at >= $%d", len(args)+1)
	args = append(args, since)

	q := "SELECT date_trunc('day', created_at) AS day, COALESCE(SUM(cost_usd), 0) " +
		"FROM token_events WHERE " + where + " GROUP BY day ORDER BY day"

	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("forecast: daily buckets: %w", err)
	}
	defer rows.Close()
	var out []DayBucket
	for rows.Next() {
		var d DayBucket
		if err := rows.Scan(&d.Day, &d.SpendUSD); err != nil {
			return nil, fmt.Errorf("forecast: scan bucket: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
