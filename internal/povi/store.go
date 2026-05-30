package povi

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgxDB is the minimal DB seam (matches budgets/store.go) so tests can inject
// pgxmock and a nil pool degrades gracefully.
type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the receipts audit trail. Every verified-or-not receipt Lens
// receives is recorded here for audit (the table is the source of truth for
// "what did each node attest to"), independent of whether any LENS was minted.
type Store struct {
	pool pgxDB
}

// NewStore guards the typed-nil interface trap; a nil pool yields a no-op store.
func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &Store{pool: db}
}

func newStore(pool pgxDB) *Store { return &Store{pool: pool} }

// StoredReceipt is the audit-view row (merkle root hex-encoded; no raw
// signature bytes — the audit trail records WHAT was attested + whether the
// signature verified, not the signature itself).
type StoredReceipt struct {
	RequestID     string    `json:"request_id"`
	NodeID        string    `json:"node_id"`
	WorkspaceID   string    `json:"workspace_id"`
	Model         string    `json:"model"`
	InputTokens   int       `json:"input_tokens"`
	OutputTokens  int       `json:"output_tokens"`
	MerkleRootHex string    `json:"merkle_root"`
	Verified      bool      `json:"verified"`
	Timestamp     int64     `json:"timestamp"`
	CreatedAt     time.Time `json:"created_at"`
}

const insertReceiptSQL = `INSERT INTO povi_receipts
    (request_id, node_id, workspace_id, model, input_tokens, output_tokens, merkle_root, verified, timestamp)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (request_id) DO NOTHING`

const listReceiptsSQL = `SELECT request_id, node_id, workspace_id, model,
    input_tokens, output_tokens, merkle_root, verified, timestamp, created_at
FROM povi_receipts WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT $2`

const statsSQL = `SELECT COUNT(*), COUNT(*) FILTER (WHERE verified) FROM povi_receipts`

// RecordReceipt appends a receipt to the audit trail with its verification
// outcome. Idempotent per request_id. No-op on a nil pool.
func (s *Store) RecordReceipt(ctx context.Context, r Receipt, verified bool) error {
	if s.pool == nil {
		return nil
	}
	rootHex := hex.EncodeToString(r.MerkleRoot[:])
	if _, err := s.pool.Exec(ctx, insertReceiptSQL,
		r.RequestID, r.NodeID, r.WorkspaceID, r.Model,
		r.InputTokens, r.OutputTokens, rootHex, verified, r.Timestamp,
	); err != nil {
		return fmt.Errorf("povi: insert receipt: %w", err)
	}
	return nil
}

// ListReceipts returns recent receipts for a workspace (audit view).
func (s *Store) ListReceipts(ctx context.Context, workspaceID string, limit int) ([]StoredReceipt, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, listReceiptsSQL, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredReceipt
	for rows.Next() {
		var sr StoredReceipt
		if err := rows.Scan(&sr.RequestID, &sr.NodeID, &sr.WorkspaceID, &sr.Model,
			&sr.InputTokens, &sr.OutputTokens, &sr.MerkleRootHex, &sr.Verified, &sr.Timestamp, &sr.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sr)
	}
	return out, rows.Err()
}

// Stats returns the total receipts recorded and how many verified — surfaced by
// the /v1/povi/status endpoint.
func (s *Store) Stats(ctx context.Context) (total, verified int, err error) {
	if s.pool == nil {
		return 0, 0, nil
	}
	err = s.pool.QueryRow(ctx, statsSQL).Scan(&total, &verified)
	return total, verified, err
}
