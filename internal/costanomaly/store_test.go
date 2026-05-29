package costanomaly

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestUnitCosts_IssueReadsRequestAttribution(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer pool.Close()
	st := newStore(pool)
	since := time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC)

	// Issue units come from request_attribution, grouped by issue_id.
	pool.ExpectQuery(`SUM\(cost_usd\).*FROM request_attribution.*issue_id <> ''.*GROUP BY issue_id`).
		WithArgs("ws1", since).
		WillReturnRows(pgxmock.NewRows([]string{"issue_id", "sum"}).
			AddRow("ENG-1", float64(12)).
			AddRow("ENG-88", float64(60)))

	got, err := st.UnitCosts(context.Background(), "ws1", UnitIssue, since)
	if err != nil {
		t.Fatalf("UnitCosts: %v", err)
	}
	if len(got) != 2 || got[1].UnitID != "ENG-88" || got[1].CostUSD != 60 {
		t.Fatalf("unexpected issue costs: %+v", got)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestUnitCosts_TeamReadsTokenEvents(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer pool.Close()
	st := newStore(pool)
	since := time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC)

	// Team units come from token_events grouped by the "team" column.
	pool.ExpectQuery(`SELECT team, SUM\(cost_usd\).*FROM token_events.*GROUP BY team`).
		WithArgs("ws1", since).
		WillReturnRows(pgxmock.NewRows([]string{"team", "sum"}).
			AddRow("core", float64(100)))

	got, err := st.UnitCosts(context.Background(), "ws1", UnitTeam, since)
	if err != nil {
		t.Fatalf("UnitCosts: %v", err)
	}
	if len(got) != 1 || got[0].UnitID != "core" {
		t.Fatalf("unexpected team costs: %+v", got)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestUnitCosts_SprintUsesSprintColumn(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer pool.Close()
	st := newStore(pool)
	since := time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC)

	pool.ExpectQuery(`SELECT sprint_id, SUM\(cost_usd\).*FROM token_events.*GROUP BY sprint_id`).
		WithArgs("ws1", since).
		WillReturnRows(pgxmock.NewRows([]string{"sprint_id", "sum"}))

	if _, err := st.UnitCosts(context.Background(), "ws1", UnitSprint, since); err != nil {
		t.Fatalf("UnitCosts: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestUnitCosts_UnknownKindErrors(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer pool.Close()
	st := newStore(pool) // non-nil db so we reach the switch
	// No ExpectQuery: an unknown kind must be rejected before any query.
	if _, err := st.UnitCosts(context.Background(), "ws1", "bogus", time.Now()); err == nil {
		t.Fatal("unknown unit kind should return an error, not query")
	}
}
