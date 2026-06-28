package benchprobe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the verifier-private pool + per-node score + never-reuse probe ledger over the 0068
// tables. It holds no ledger and reaches no mint path.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// SeedItem inserts/updates one verifier-private pool item (the operator seed tool's only write).
func (s *Store) SeedItem(ctx context.Context, item EvalItem) error {
	method := item.EvalMethod
	if method == "" {
		method = "exact"
	}
	thr := item.PassThreshold
	if thr == 0 {
		thr = 1.0
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO benchmark_eval_items (id, input, expected_output, eval_method, pass_threshold, active)
		 VALUES ($1,$2,$3,$4,$5,true)
		 ON CONFLICT (id) DO UPDATE SET input=$2, expected_output=$3, eval_method=$4, pass_threshold=$5`,
		item.ID, item.Input, item.ExpectedOutput, method, thr)
	if err != nil {
		return fmt.Errorf("benchprobe: seed item: %w", err)
	}
	return nil
}

// DrawItem picks an UNPREDICTABLE active item NOT yet probed to nodeID. It loads the candidate id set
// (active, never-probed-to-this-node) and selects one via crypto/rand (the unpredictability source,
// mirroring the PoVI challenger) so the node cannot anticipate which item it will be asked. Returns
// nil when the pool is exhausted for this node (no item left to draw) — the caller treats nil as a
// no-op, never an error.
func (s *Store) DrawItem(ctx context.Context, nodeID string) (*EvalItem, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM benchmark_eval_items
		 WHERE active AND id NOT IN (SELECT item_id FROM benchmark_probes WHERE node_id = $1)`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("benchprobe: draw candidates: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil // pool exhausted for this node
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(ids))))
	if err != nil {
		return nil, fmt.Errorf("benchprobe: crypto/rand draw: %w", err)
	}
	pick := ids[n.Int64()]

	var it EvalItem
	if err := s.pool.QueryRow(ctx,
		`SELECT id, input, expected_output, eval_method, pass_threshold FROM benchmark_eval_items WHERE id=$1`, pick).
		Scan(&it.ID, &it.Input, &it.ExpectedOutput, &it.EvalMethod, &it.PassThreshold); err != nil {
		return nil, fmt.Errorf("benchprobe: load item: %w", err)
	}
	return &it, nil
}

// RecordProbe inserts the never-reuse ledger row. ON CONFLICT (node_id, item_id) DO NOTHING makes a
// double-draw race idempotent (the UNIQUE constraint is the real never-reuse guarantee).
func (s *Store) RecordProbe(ctx context.Context, p Probe) error {
	id := p.ID
	if id == "" {
		id = newID()
	}
	var reqID any
	if p.RequestID != "" {
		reqID = p.RequestID
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO benchmark_probes (id, node_id, item_id, request_id, score)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (node_id, item_id) DO NOTHING`,
		id, p.NodeID, p.ItemID, reqID, p.Score)
	if err != nil {
		return fmt.Errorf("benchprobe: record probe: %w", err)
	}
	return nil
}

// UpsertNodeScore folds one probe score into the node's running average for that model.
func (s *Store) UpsertNodeScore(ctx context.Context, nodeID, model string, score float64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO benchmark_node_scores (node_id, model, score, sample_count, updated_at)
		 VALUES ($1,$2,$3,1,NOW())
		 ON CONFLICT (node_id, model) DO UPDATE SET
		   score = (benchmark_node_scores.score * benchmark_node_scores.sample_count + $3)
		           / (benchmark_node_scores.sample_count + 1),
		   sample_count = benchmark_node_scores.sample_count + 1,
		   updated_at = NOW()`,
		nodeID, model, score)
	if err != nil {
		return fmt.Errorf("benchprobe: upsert node score: %w", err)
	}
	return nil
}

// NodeScore reads the current per-node score + sample count (for routing in PR-B + tests). 0,0 when absent.
func (s *Store) NodeScore(ctx context.Context, nodeID, model string) (score float64, sampleCount int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT score, sample_count FROM benchmark_node_scores WHERE node_id=$1 AND model=$2`, nodeID, model).
		Scan(&score, &sampleCount)
	if err != nil {
		return 0, 0, nil // absent ⇒ unscored
	}
	return score, sampleCount, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
