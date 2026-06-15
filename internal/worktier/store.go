package worktier

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// execer is the write surface the store needs — Exec ONLY. It deliberately
// exposes NO Begin and NO Query, so the write path has no handle that could
// reach a ledger credit. This is the architectural half of the mint-free
// guarantee (the import guard test is the other half: this package imports no
// mining/economy/poolroyalty/povi).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the read surface the aggregate needs — Query ONLY, also no Begin.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store persists WorkTier observations (write) and serves the per-workspace
// aggregate (read). It holds ONLY an execer/queryer — never a *LedgerStore, never
// a Begin — so no credit path is reachable. A nil-pool store is a no-op (tests).
type Store struct {
	db  execer
	rdb queryer
}

// NewStore wraps a pool. nil → a no-op store (Record/Aggregate do nothing).
func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		return &Store{}
	}
	return &Store{db: pool, rdb: pool}
}

const insertWorkTierSQL = `INSERT INTO work_tier_observations
    (workspace_id, feature, model, provider, size_bucket, cost_bucket, complexity, sensitivity,
     input_tokens, output_tokens, cost_usd, complexity_score, pii_detected, guardrail_fired)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`

// Record persists one observation: the 4 derived buckets + the RAW signal behind
// each. NON-CONTENT only. No-op when no db is wired.
func (s *Store) Record(ctx context.Context, workspaceID, feature, model, provider string, wt WorkTier,
	inputTokens, outputTokens int, costUSD float64, complexityScore int, piiDetected, guardrailFired bool) error {
	if s == nil || s.db == nil {
		return nil
	}
	if _, err := s.db.Exec(ctx, insertWorkTierSQL,
		workspaceID, feature, model, provider,
		string(wt.Size), string(wt.Cost), string(wt.Complexity), string(wt.Sensitivity),
		inputTokens, outputTokens, costUSD, complexityScore, piiDetected, guardrailFired); err != nil {
		return fmt.Errorf("worktier: record observation: %w", err)
	}
	return nil
}

// TierCount is one (model, tier) cell of the per-workspace distribution.
type TierCount struct {
	Model       string
	Size        SizeBucket
	Cost        CostBucket
	Complexity  Complexity
	Sensitivity Sensitivity
	Count       int
}

const aggregateSQL = `SELECT model, size_bucket, cost_bucket, complexity, sensitivity, COUNT(*)
FROM work_tier_observations
WHERE workspace_id = $1 AND created_at > now() - ($2::bigint * interval '1 microsecond')
GROUP BY model, size_bucket, cost_bucket, complexity, sensitivity
ORDER BY COUNT(*) DESC, model`

// Aggregate returns the per-workspace tier distribution over the window, SLICED
// BY MODEL (the Advisor's quality-per-dollar-per-model need). Per-workspace
// scoped (WHERE workspace_id=$1) — never cross-tenant. nil store → empty.
func (s *Store) Aggregate(ctx context.Context, workspaceID string, window time.Duration) ([]TierCount, error) {
	if s == nil || s.rdb == nil {
		return nil, nil
	}
	rows, err := s.rdb.Query(ctx, aggregateSQL, workspaceID, window.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("worktier: aggregate: %w", err)
	}
	defer rows.Close()
	var out []TierCount
	for rows.Next() {
		var tc TierCount
		if err := rows.Scan(&tc.Model, &tc.Size, &tc.Cost, &tc.Complexity, &tc.Sensitivity, &tc.Count); err != nil {
			return nil, fmt.Errorf("worktier: scan aggregate row: %w", err)
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}
