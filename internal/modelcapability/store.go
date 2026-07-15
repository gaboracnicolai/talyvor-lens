package modelcapability

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/worktier"
)

// execer is the write surface — Exec ONLY. No Begin, no Query: the write path has
// no handle that could reach a ledger credit. The architectural half of the
// mint-free guarantee (the import guard is the other half).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the read surface — Query ONLY, also no Begin.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store records model-capability observations (write) and fits per-model curves
// (read). Holds ONLY an execer/queryer — never a *LedgerStore, never a Begin — so
// no credit path is reachable. A nil-pool store is inert (tests / default-off).
type Store struct {
	db  execer
	rdb queryer
}

// NewStore wraps a pool. nil → a no-op store.
func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		return &Store{}
	}
	return &Store{db: pool, rdb: pool}
}

const insertSQL = `INSERT INTO model_capability_observations (model, provider, difficulty, quality)
VALUES ($1,$2,$3,$4)`

// Record persists one capability observation: a (model, provider) served a request
// at the given H1-derived difficulty and scored the given composite quality [0,1].
// NON-CONTENT. No-op when no db is wired.
func (s *Store) Record(ctx context.Context, model, provider string, difficulty int, quality float64) error {
	if s == nil || s.db == nil {
		return nil
	}
	if _, err := s.db.Exec(ctx, insertSQL, model, provider, difficulty, quality); err != nil {
		return fmt.Errorf("modelcapability: record: %w", err)
	}
	return nil
}

// RecordServed is the H1-bound entry: it derives the difficulty ordinal from H1's
// WorkTier classification of the served request, then records it. This is the
// explicit "H2 consumes H1's tier output" seam on the write side.
func (s *Store) RecordServed(ctx context.Context, model, provider string, wt worktier.WorkTier, quality float64) error {
	return s.Record(ctx, model, provider, DifficultyOfWorkTier(wt), quality)
}

const fitSQL = `SELECT model, provider, difficulty, AVG(quality), COUNT(*)
FROM model_capability_observations
GROUP BY model, provider, difficulty
ORDER BY model, provider, difficulty`

// Fit builds, per (model, provider), the capability curve: the per-difficulty mean
// quality points plus a linear quality-vs-difficulty trend (Slope < 0 ⇒ quality
// degrades as work-tier rises; Slope ≈ 0 ⇒ it holds). Read-only. nil store → nil.
func (s *Store) Fit(ctx context.Context) ([]Curve, error) {
	if s == nil || s.rdb == nil {
		return nil, nil
	}
	rows, err := s.rdb.Query(ctx, fitSQL)
	if err != nil {
		return nil, fmt.Errorf("modelcapability: fit query: %w", err)
	}
	defer rows.Close()

	var curves []Curve
	var cur *Curve // the curve currently being accumulated (rows are ordered by model, provider)
	flush := func() {
		if cur == nil {
			return
		}
		cur.Slope, cur.Intercept = fitLinear(cur.Points)
		curves = append(curves, *cur)
		cur = nil
	}
	for rows.Next() {
		var model, provider string
		var difficulty, samples int
		var avgQuality float64
		if err := rows.Scan(&model, &provider, &difficulty, &avgQuality, &samples); err != nil {
			return nil, fmt.Errorf("modelcapability: scan fit row: %w", err)
		}
		if cur == nil || cur.Model != model || cur.Provider != provider {
			flush()
			cur = &Curve{Model: model, Provider: provider}
		}
		cur.Points = append(cur.Points, CurvePoint{Difficulty: difficulty, AvgQuality: avgQuality, Samples: samples})
		cur.N += samples
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("modelcapability: fit rows: %w", err)
	}
	flush()
	return curves, nil
}
