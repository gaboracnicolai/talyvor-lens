// Package nodelatency is the DESCRIPTIVE per-(node, cohort) latency aggregate for proof-of-latency-locality
// (P3 #6). It captures gateway-measured node serve latency into a rolling EWMA aggregate that a LATER mint
// (PR-L4) reads to reward genuinely-fast nodes (cohort-relative + cost-weighted, quality-gated by the
// existing benchmark_node_scores). This package is MINT-FREE by construction: the store exposes only an
// Exec surface (no Begin, no ledger), and it imports no mining/poolroyalty/economy/povi (import-guard test).
package nodelatency

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// latencyEMAWeight (α) controls how fast latency_ewma reacts to recent service. 0.2 matches the codebase
// precedent (localrouter.latencyEMAWeight, compute_mining's (avg*4+new)/5) — recent latency dominates so a
// node cannot bank ancient-fast history, without a single slow request swinging the stat.
const latencyEMAWeight = 0.2

// execer is the write surface — Exec ONLY. No Begin, no Query: the write path holds no handle that could
// reach a ledger credit (the architectural half of the mint-free guarantee; the import guard is the other).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store persists node-latency observations into the rolling per-(node,cohort) aggregate. It holds ONLY an
// execer — never a *LedgerStore, never a Begin — so no credit path is reachable. A nil-pool store is a no-op.
type Store struct {
	db execer
}

// NewStore wraps a pool. nil → a no-op store (RecordServe does nothing).
func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		return &Store{}
	}
	return &Store{db: pool}
}

// recordServeSQL folds one gateway-measured serve into the (node,cohort) EWMA. First observation seeds the
// raw latency + cost + count=1; subsequent ones blend latency via α (EXCLUDED.latency_ewma is the new raw
// latency, $7 = α) and accumulate the cost weight. Composition (ii): RAW latency + cost stored, not
// pre-normalized — the mint composes the cost-weighted cohort-relative score.
// The α weights are baked as SQL literals (single-sourced from latencyEMAWeight via Sprintf) rather than a
// bound param: under the simple protocol, a param in `1 - $n` gets type-inferred as INTEGER and truncates
// 0.2→0. The weight is a TRUSTED internal const (never user input), so the interpolation is injection-safe
// (the batch-limit Sprintf pattern the minters use).
var recordServeSQL = fmt.Sprintf(`INSERT INTO node_cohort_latency_stats
    (node_id, feature_category, input_token_range, complexity_bucket, latency_ewma, cost_weight_accum, sample_count, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, 1, now())
ON CONFLICT (node_id, feature_category, input_token_range, complexity_bucket) DO UPDATE SET
    latency_ewma      = node_cohort_latency_stats.latency_ewma * %[1]g + EXCLUDED.latency_ewma * %[2]g,
    cost_weight_accum = node_cohort_latency_stats.cost_weight_accum + EXCLUDED.cost_weight_accum,
    sample_count      = node_cohort_latency_stats.sample_count + 1,
    updated_at        = now()`, 1-latencyEMAWeight, latencyEMAWeight)

// RecordServe folds one gateway-measured serve (latencyMs) for nodeID on the given cohort into the EWMA
// aggregate, accumulating the request's cost weight (AnalyseComplexity score). No-op when no db is wired.
// Descriptive: writes ONLY node_cohort_latency_stats, credits nothing.
func (s *Store) RecordServe(ctx context.Context, nodeID, featureCategory, inputTokenRange, complexityBucket string, latencyMs int64, costScore int) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.Exec(ctx, recordServeSQL,
		nodeID, featureCategory, inputTokenRange, complexityBucket,
		float64(latencyMs), float64(costScore))
	if err != nil {
		return fmt.Errorf("nodelatency: record serve: %w", err)
	}
	return nil
}
