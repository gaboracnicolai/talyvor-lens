package povi

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NodeStakeStore is the povi_stakes persistence (the real stakeStore impl). It
// reuses the package pgxDB seam so tests inject pgxmock and a nil pool degrades
// gracefully.
type NodeStakeStore struct {
	pool pgxDB
}

// NewNodeStakeStore guards the typed-nil interface trap; a nil pool yields a
// no-op store.
func NewNodeStakeStore(pool *pgxpool.Pool) *NodeStakeStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &NodeStakeStore{pool: db}
}

func newNodeStakeStore(pool pgxDB) *NodeStakeStore { return &NodeStakeStore{pool: pool} }

const upsertStakeSQL = `INSERT INTO povi_stakes
    (node_id, workspace_id, amount, status, slashed_amount, locked_at, unbond_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (node_id) DO UPDATE SET
    workspace_id = EXCLUDED.workspace_id,
    amount = EXCLUDED.amount,
    status = EXCLUDED.status,
    slashed_amount = EXCLUDED.slashed_amount,
    locked_at = EXCLUDED.locked_at,
    unbond_at = EXCLUDED.unbond_at,
    updated_at = EXCLUDED.updated_at`

const selectStakeSQL = `SELECT node_id, workspace_id, amount, status, slashed_amount, locked_at, unbond_at, updated_at
FROM povi_stakes WHERE node_id = $1`

const listStakesSQL = `SELECT node_id, workspace_id, amount, status, slashed_amount, locked_at, unbond_at, updated_at
FROM povi_stakes ORDER BY updated_at DESC`

// Put upserts a stake row (one row per node).
func (s *NodeStakeStore) Put(ctx context.Context, st Stake) error {
	if s.pool == nil {
		return nil
	}
	if _, err := s.pool.Exec(ctx, upsertStakeSQL,
		st.NodeID, st.WorkspaceID, st.Amount, string(st.Status),
		st.SlashedAmount, st.LockedAt, st.UnbondAt, st.UpdatedAt,
	); err != nil {
		return fmt.Errorf("povi: upsert stake: %w", err)
	}
	return nil
}

// Get returns a node's stake, or (nil, nil) when none exists.
func (s *NodeStakeStore) Get(ctx context.Context, nodeID string) (*Stake, error) {
	if s.pool == nil {
		return nil, nil
	}
	var st Stake
	var status string
	err := s.pool.QueryRow(ctx, selectStakeSQL, nodeID).Scan(
		&st.NodeID, &st.WorkspaceID, &st.Amount, &status,
		&st.SlashedAmount, &st.LockedAt, &st.UnbondAt, &st.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	st.Status = StakeStatus(status)
	return &st, nil
}

// GetTx reads a stake row within an external transaction.
func (s *NodeStakeStore) GetTx(ctx context.Context, tx pgx.Tx, nodeID string) (*Stake, error) {
	var st Stake
	var status string
	err := tx.QueryRow(ctx, selectStakeSQL, nodeID).Scan(
		&st.NodeID, &st.WorkspaceID, &st.Amount, &status,
		&st.SlashedAmount, &st.LockedAt, &st.UnbondAt, &st.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	st.Status = StakeStatus(status)
	return &st, nil
}

// PutTx upserts a stake row within an external transaction.
func (s *NodeStakeStore) PutTx(ctx context.Context, tx pgx.Tx, st Stake) error {
	if _, err := tx.Exec(ctx, upsertStakeSQL,
		st.NodeID, st.WorkspaceID, st.Amount, string(st.Status),
		st.SlashedAmount, st.LockedAt, st.UnbondAt, st.UpdatedAt,
	); err != nil {
		return fmt.Errorf("povi: upsert stake (tx): %w", err)
	}
	return nil
}

// List returns all stakes, newest-updated first.
func (s *NodeStakeStore) List(ctx context.Context) ([]Stake, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, listStakesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stake
	for rows.Next() {
		var st Stake
		var status string
		if err := rows.Scan(&st.NodeID, &st.WorkspaceID, &st.Amount, &status,
			&st.SlashedAmount, &st.LockedAt, &st.UnbondAt, &st.UpdatedAt); err != nil {
			return nil, err
		}
		st.Status = StakeStatus(status)
		out = append(out, st)
	}
	return out, rows.Err()
}
