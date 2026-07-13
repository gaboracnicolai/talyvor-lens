package povi

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestChallengeStore_Record(t *testing.T) {
	pool := newStorePool(t)
	pool.ExpectExec(`INSERT INTO povi_challenges`).
		WithArgs("chal_1", "req-1", "node-1", "ws-op", "3,7,11", "fail", int64(50), "challenge_fail:req-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	s := newChallengeStore(pool)
	err := s.Record(context.Background(), Challenge{
		ID: "chal_1", RequestID: "req-1", NodeID: "node-1", WorkspaceID: "ws-op",
		Positions: []int{3, 7, 11}, Result: ChallengeFail, SlashedAmount: 50,
		Reason: "challenge_fail:req-1", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestChallengeStore_AlreadyChallenged(t *testing.T) {
	pool := newStorePool(t)
	pool.ExpectQuery(`SELECT EXISTS`).WithArgs("req-1").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))

	s := newChallengeStore(pool)
	done, err := s.AlreadyChallenged(context.Background(), "req-1")
	if err != nil || !done {
		t.Errorf("AlreadyChallenged = %v, %v; want true, nil", done, err)
	}
}

func TestChallengeStore_GetParsesPositions(t *testing.T) {
	pool := newStorePool(t)
	rows := pgxmock.NewRows([]string{
		"id", "request_id", "node_id", "workspace_id", "positions", "result", "slashed_amount", "reason", "created_at",
	}).AddRow("chal_1", "req-1", "node-1", "ws-op", "3,7,11", "fail", int64(50), "r", time.Now())
	pool.ExpectQuery(`FROM povi_challenges WHERE id`).WithArgs("chal_1").WillReturnRows(rows)

	s := newChallengeStore(pool)
	ch, err := s.Get(context.Background(), "chal_1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ch == nil || len(ch.Positions) != 3 || ch.Positions[1] != 7 || ch.Result != ChallengeFail {
		t.Errorf("challenge = %+v", ch)
	}
}

func TestChallengeStore_NilPoolSafe(t *testing.T) {
	s := NewChallengeStore(nil)
	if err := s.Record(context.Background(), Challenge{ID: "x", RequestID: "r"}); err != nil {
		t.Errorf("nil-pool Record should no-op: %v", err)
	}
	if done, err := s.AlreadyChallenged(context.Background(), "r"); done || err != nil {
		t.Errorf("nil-pool AlreadyChallenged = %v,%v", done, err)
	}
}
