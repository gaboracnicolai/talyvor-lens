package povi

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MeasurementStore is Lens's own record of what each node served for a request —
// the GATEWAY MEASUREMENT that the receipt→LENS mint is priced on. The proxy
// writes one row per node-served request (request_id → node_id + Lens-measured
// output tokens) at dispatch time; MintFromReceipt reads it back to price the
// mint on the measurement and bind it to the serving node. Satisfies
// MeasurementLookup. Same pgxDB seam + nil-pool safety as Store.
type MeasurementStore struct {
	pool pgxDB
}

// NewMeasurementStore guards the typed-nil interface trap; a nil pool yields a
// no-op store (Record no-ops, ServedMeasurement returns nil → mint fails closed).
func NewMeasurementStore(pool *pgxpool.Pool) *MeasurementStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &MeasurementStore{pool: db}
}

func newMeasurementStore(pool pgxDB) *MeasurementStore { return &MeasurementStore{pool: pool} }

const insertMeasurementSQL = `INSERT INTO served_request_measurements
    (request_id, node_id, workspace_id, output_tokens)
VALUES ($1, $2, $3, $4)
ON CONFLICT (request_id) DO NOTHING`

const getMeasurementSQL = `SELECT node_id, output_tokens
FROM served_request_measurements WHERE request_id = $1`

// Record persists Lens's measurement of one node-served request. Idempotent per
// request_id (ON CONFLICT DO NOTHING) — a retry/duplicate dispatch keeps the
// FIRST measurement, so the mint basis for a request_id can't be rewritten. A
// nil pool no-ops (degraded mode / tests).
func (s *MeasurementStore) Record(ctx context.Context, requestID, nodeID, workspaceID string, outputTokens int) error {
	if s.pool == nil {
		return nil
	}
	if _, err := s.pool.Exec(ctx, insertMeasurementSQL, requestID, nodeID, workspaceID, outputTokens); err != nil {
		return fmt.Errorf("povi: insert served measurement: %w", err)
	}
	return nil
}

// ServedMeasurement returns Lens's measurement for a request_id, or (nil, nil)
// when Lens has NO record of it — the fail-closed signal MintFromReceipt turns
// into ErrNoServedMeasurement (mint nothing). A nil pool returns (nil, nil) too.
func (s *MeasurementStore) ServedMeasurement(ctx context.Context, requestID string) (*ServedMeasurement, error) {
	if s.pool == nil {
		return nil, nil
	}
	var m ServedMeasurement
	err := s.pool.QueryRow(ctx, getMeasurementSQL, requestID).Scan(&m.NodeID, &m.OutputTokens)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("povi: read served measurement: %w", err)
	}
	return &m, nil
}
