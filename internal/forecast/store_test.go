package forecast

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/budgets"
)

func TestDailyBuckets_WorkspaceScope(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer pool.Close()
	st := newStore(pool, nil)

	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	// Workspace scope: no team/sprint predicate — just workspace_id + window.
	pool.ExpectQuery(`date_trunc\('day', created_at\).*FROM token_events WHERE workspace_id = \$1 AND created_at >= \$2 GROUP BY day`).
		WithArgs("ws1", now.AddDate(0, 0, -7)).
		WillReturnRows(pgxmock.NewRows([]string{"day", "sum"}).
			AddRow(time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC), float64(10)).
			AddRow(time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC), float64(20)))

	got, err := st.DailyBuckets(context.Background(), "ws1", budgets.ScopeWorkspace, "ws1", 7, now)
	if err != nil {
		t.Fatalf("DailyBuckets: %v", err)
	}
	if len(got) != 2 || got[0].SpendUSD != 10 || got[1].SpendUSD != 20 {
		t.Fatalf("unexpected buckets: %+v", got)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestDailyBuckets_TeamScopeAddsColumn(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer pool.Close()
	st := newStore(pool, nil)
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)

	// Team scope reuses budgets.ScopeColumn → "team = $2".
	pool.ExpectQuery(`WHERE workspace_id = \$1 AND team = \$2 AND created_at >= \$3`).
		WithArgs("ws1", "teamA", now.AddDate(0, 0, -7)).
		WillReturnRows(pgxmock.NewRows([]string{"day", "sum"}).
			AddRow(time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC), float64(5)))

	got, err := st.DailyBuckets(context.Background(), "ws1", budgets.ScopeTeam, "teamA", 7, now)
	if err != nil {
		t.Fatalf("DailyBuckets: %v", err)
	}
	if len(got) != 1 || got[0].SpendUSD != 5 {
		t.Fatalf("unexpected buckets: %+v", got)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestDailyBuckets_SprintScopeUsesSprintColumn(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer pool.Close()
	st := newStore(pool, nil)
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)

	pool.ExpectQuery(`WHERE workspace_id = \$1 AND sprint_id = \$2 AND created_at >= \$3`).
		WithArgs("ws1", "sp1", now.AddDate(0, 0, -14)).
		WillReturnRows(pgxmock.NewRows([]string{"day", "sum"}))

	if _, err := st.DailyBuckets(context.Background(), "ws1", budgets.ScopeSprint, "sp1", 14, now); err != nil {
		t.Fatalf("DailyBuckets: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestStore_NilPoolReturnsEmpty(t *testing.T) {
	st := NewStore(nil)
	got, err := st.DailyBuckets(context.Background(), "ws1", budgets.ScopeWorkspace, "ws1", 7, time.Now())
	if err != nil || got != nil {
		t.Fatalf("nil pool should return (nil,nil): got %v err %v", got, err)
	}
}
