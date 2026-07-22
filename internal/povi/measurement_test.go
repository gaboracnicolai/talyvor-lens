package povi

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func TestMeasurementStore_Record_Inserts(t *testing.T) {
	pool := newStorePool(t)
	pool.ExpectExec(`INSERT INTO served_request_measurements`).
		WithArgs("req-1", "node-1", "ws-1", 500).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	s := newMeasurementStore(pool)
	if err := s.Record(context.Background(), "req-1", "node-1", "ws-1", 500); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMeasurementStore_ServedMeasurement_ReturnsBoundRow(t *testing.T) {
	pool := newStorePool(t)
	pool.ExpectQuery(`FROM served_request_measurements WHERE request_id`).
		WithArgs("req-1").
		WillReturnRows(pgxmock.NewRows([]string{"node_id", "output_tokens"}).AddRow("node-1", 500))

	s := newMeasurementStore(pool)
	m, err := s.ServedMeasurement(context.Background(), "req-1")
	if err != nil {
		t.Fatalf("ServedMeasurement: %v", err)
	}
	if m == nil || m.NodeID != "node-1" || m.OutputTokens != 500 {
		t.Errorf("measurement = %+v, want {node-1, 500}", m)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// FAIL CLOSED at the source: no row for the request_id ⇒ (nil, nil), which
// MintFromReceipt turns into ErrNoServedMeasurement (mint nothing).
func TestMeasurementStore_ServedMeasurement_NoRow_ReturnsNil(t *testing.T) {
	pool := newStorePool(t)
	pool.ExpectQuery(`FROM served_request_measurements WHERE request_id`).
		WithArgs("req-unknown").
		WillReturnError(pgx.ErrNoRows)

	s := newMeasurementStore(pool)
	m, err := s.ServedMeasurement(context.Background(), "req-unknown")
	if err != nil {
		t.Fatalf("no-row must be (nil, nil), got err %v", err)
	}
	if m != nil {
		t.Errorf("measurement = %+v, want nil for an unknown request", m)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A nil-pool measurement store is safe: Record no-ops, ServedMeasurement returns
// (nil, nil) — so an enabled mint fails closed rather than crashing when no DB.
func TestMeasurementStore_NilPoolIsSafe(t *testing.T) {
	s := NewMeasurementStore(nil)
	if err := s.Record(context.Background(), "r", "n", "w", 1); err != nil {
		t.Errorf("nil-pool Record should no-op, got %v", err)
	}
	m, err := s.ServedMeasurement(context.Background(), "r")
	if err != nil || m != nil {
		t.Errorf("nil-pool ServedMeasurement = %v, %v; want nil,nil", m, err)
	}
}
