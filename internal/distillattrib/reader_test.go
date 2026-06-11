package distillattrib

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestReader_RawRows_QueriesTableWithLimit(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	r := NewReader(mock)

	now := time.Now()
	cols := []string{"owner", "requester", "hash", "serve_count", "first", "last"}
	mock.ExpectQuery(`FROM distill_serve_attribution`).
		WithArgs(50).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("wsA", "wsB", "h1", int64(7), now, now))

	rows, err := r.RawRows(context.Background(), 50)
	if err != nil {
		t.Fatalf("RawRows: %v", err)
	}
	if len(rows) != 1 || rows[0].OwnerWorkspaceID != "wsA" || rows[0].RequesterWorkspaceID != "wsB" ||
		rows[0].ContentHash != "h1" || rows[0].ServeCount != 7 {
		t.Fatalf("RawRows = %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("RawRows must SELECT FROM distill_serve_attribution with limit $1: %v", err)
	}
}

func TestReader_PairTotals_GroupsByOwnerRequester(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	r := NewReader(mock)

	now := time.Now()
	// The condition-(b) probe MUST aggregate SUM(serve_count) GROUP BY the pair.
	mock.ExpectQuery(`SUM\(serve_count\)[\s\S]*GROUP BY owner_workspace_id, requester_workspace_id`).
		WithArgs(100).
		WillReturnRows(pgxmock.NewRows([]string{"owner", "requester", "serves", "last"}).
			AddRow("wsA", "wsB", int64(42), now))

	pairs, err := r.PairTotals(context.Background(), 100)
	if err != nil {
		t.Fatalf("PairTotals: %v", err)
	}
	if len(pairs) != 1 || pairs[0].OwnerWorkspaceID != "wsA" || pairs[0].Serves != 42 {
		t.Fatalf("PairTotals = %+v", pairs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("PairTotals must GROUP BY (owner, requester) with SUM(serve_count): %v", err)
	}
}

func TestReader_NilDBInert(t *testing.T) {
	r := NewReader(nil) // nil db → inert, no panic, empty results
	if rows, err := r.RawRows(context.Background(), 10); err != nil || rows != nil {
		t.Fatalf("inert RawRows = (%v, %v), want (nil, nil)", rows, err)
	}
	if pairs, err := r.PairTotals(context.Background(), 10); err != nil || pairs != nil {
		t.Fatalf("inert PairTotals = (%v, %v), want (nil, nil)", pairs, err)
	}
}
