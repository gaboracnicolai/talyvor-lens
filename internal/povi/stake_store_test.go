package povi

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func TestNodeStakeStore_PutUpserts(t *testing.T) {
	pool := newStorePool(t)
	now := time.Now()
	pool.ExpectExec(`INSERT INTO povi_stakes`).
		WithArgs("node-1", "ws-1", 100.0, "active", 0.0, now, pgxmock.AnyArg(), now).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	s := newNodeStakeStore(pool)
	err := s.Put(context.Background(), Stake{
		NodeID: "node-1", WorkspaceID: "ws-1", Amount: 100, Status: StakeActive,
		LockedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestNodeStakeStore_GetScansRow(t *testing.T) {
	pool := newStorePool(t)
	now := time.Now()
	rows := pgxmock.NewRows([]string{
		"node_id", "workspace_id", "amount", "status", "slashed_amount", "locked_at", "unbond_at", "updated_at",
	}).AddRow("node-1", "ws-1", 60.0, "active", 40.0, now, nil, now)
	pool.ExpectQuery(`FROM povi_stakes WHERE node_id`).WithArgs("node-1").WillReturnRows(rows)

	s := newNodeStakeStore(pool)
	st, err := s.Get(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st == nil || st.NodeID != "node-1" || st.Amount != 60 || st.SlashedAmount != 40 || st.Status != StakeActive {
		t.Errorf("stake = %+v", st)
	}
	if st.UnbondAt != nil {
		t.Error("unbond_at should scan to nil when NULL")
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestNodeStakeStore_GetMissingReturnsNil(t *testing.T) {
	pool := newStorePool(t)
	pool.ExpectQuery(`FROM povi_stakes WHERE node_id`).WithArgs("nope").
		WillReturnError(pgx.ErrNoRows)

	s := newNodeStakeStore(pool)
	st, err := s.Get(context.Background(), "nope")
	if err != nil || st != nil {
		t.Errorf("missing stake should be (nil,nil), got %+v, %v", st, err)
	}
}

func TestNodeStakeStore_List(t *testing.T) {
	pool := newStorePool(t)
	now := time.Now()
	rows := pgxmock.NewRows([]string{
		"node_id", "workspace_id", "amount", "status", "slashed_amount", "locked_at", "unbond_at", "updated_at",
	}).AddRow("n1", "ws", 100.0, "active", 0.0, now, nil, now).
		AddRow("n2", "ws", 0.0, "released", 0.0, now, &now, now)
	pool.ExpectQuery(`FROM povi_stakes`).WillReturnRows(rows)

	s := newNodeStakeStore(pool)
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d stakes, want 2", len(list))
	}
}

func TestNodeStakeStore_NilPoolSafe(t *testing.T) {
	s := NewNodeStakeStore(nil)
	if st, err := s.Get(context.Background(), "x"); st != nil || err != nil {
		t.Errorf("nil-pool Get = %v,%v", st, err)
	}
	if err := s.Put(context.Background(), Stake{NodeID: "x"}); err != nil {
		t.Errorf("nil-pool Put should no-op, got %v", err)
	}
}
