package povi

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newStorePool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRecordReceipt_Inserts(t *testing.T) {
	pool := newStorePool(t)
	r := sampleReceipt()
	rootHex := hex.EncodeToString(r.MerkleRoot[:])

	pool.ExpectExec(`INSERT INTO povi_receipts`).
		WithArgs(r.RequestID, r.NodeID, r.WorkspaceID, r.Model,
			r.InputTokens, r.OutputTokens, rootHex, true, r.Timestamp).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	s := newStore(pool)
	if err := s.RecordReceipt(context.Background(), r, true); err != nil {
		t.Fatalf("RecordReceipt: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestListReceipts_ParsesRows(t *testing.T) {
	pool := newStorePool(t)
	rows := pgxmock.NewRows([]string{
		"request_id", "node_id", "workspace_id", "model",
		"input_tokens", "output_tokens", "merkle_root", "verified", "timestamp", "created_at",
	}).AddRow("req-1", "node-1", "ws-1", "llama", 10, 20, "deadbeef", true, int64(1748600000), time.Now())

	pool.ExpectQuery(`FROM povi_receipts WHERE workspace_id`).WithArgs("ws-1", 50).WillReturnRows(rows)

	s := newStore(pool)
	list, err := s.ListReceipts(context.Background(), "ws-1", 50)
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
	if len(list) != 1 || list[0].RequestID != "req-1" || !list[0].Verified || list[0].MerkleRootHex != "deadbeef" {
		t.Errorf("list = %+v", list)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestStats_CountsTotalAndVerified(t *testing.T) {
	pool := newStorePool(t)
	pool.ExpectQuery(`COUNT\(\*\) FILTER \(WHERE verified\) FROM povi_receipts`).
		WillReturnRows(pgxmock.NewRows([]string{"count", "verified"}).AddRow(10, 7))

	s := newStore(pool)
	total, verified, err := s.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if total != 10 || verified != 7 {
		t.Errorf("Stats = %d total, %d verified; want 10, 7", total, verified)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A nil-pool store is safe: reads return empty, writes no-op (so PoVI degrades
// gracefully when no DB is wired).
func TestStore_NilPoolIsSafe(t *testing.T) {
	s := NewStore(nil)
	if err := s.RecordReceipt(context.Background(), sampleReceipt(), true); err != nil {
		t.Errorf("nil-pool RecordReceipt should no-op, got %v", err)
	}
	list, err := s.ListReceipts(context.Background(), "ws", 10)
	if err != nil || list != nil {
		t.Errorf("nil-pool ListReceipts = %v, %v; want nil,nil", list, err)
	}
}
