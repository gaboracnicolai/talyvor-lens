package routingbrain

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// execer / queryer are the ONLY DB seams the store holds — Exec and Query, never
// Begin. No credit path is reachable; the brain persists descriptive recommendations
// and reads them back, and can touch nothing else. (The import guard is the other
// half of the mint-free guarantee.)
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store persists the offline brain's recommendations and the per-workspace
// autonomous opt-in. Holds only execer/queryer. A nil-pool store is inert.
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

const upsertRecSQL = `INSERT INTO routing_brain_recommendations
    (workspace_id, difficulty, model, provider, expected_quality, verified, reason, computed_at)
VALUES ($1,$2,$3,$4,$5,$6,$7, NOW())
ON CONFLICT (workspace_id, difficulty) DO UPDATE SET
    model = EXCLUDED.model, provider = EXCLUDED.provider, expected_quality = EXCLUDED.expected_quality,
    verified = EXCLUDED.verified, reason = EXCLUDED.reason, computed_at = NOW()`

// Upsert writes one recommendation, replacing any prior pick for the same
// (workspace, difficulty). No-op when no db is wired.
func (s *Store) Upsert(ctx context.Context, r Recommendation) error {
	if s == nil || s.db == nil {
		return nil
	}
	if _, err := s.db.Exec(ctx, upsertRecSQL,
		r.WorkspaceID, r.Difficulty, r.Model, r.Provider, r.ExpectedQuality, r.Verified, r.Reason); err != nil {
		return fmt.Errorf("routingbrain: upsert recommendation: %w", err)
	}
	return nil
}

const loadRecsSQL = `SELECT workspace_id, difficulty, model, provider, expected_quality, verified, reason
FROM routing_brain_recommendations`

// LoadRecommendations reads every recommendation (for the in-memory serving cache).
func (s *Store) LoadRecommendations(ctx context.Context) ([]Recommendation, error) {
	if s == nil || s.rdb == nil {
		return nil, nil
	}
	rows, err := s.rdb.Query(ctx, loadRecsSQL)
	if err != nil {
		return nil, fmt.Errorf("routingbrain: load recommendations: %w", err)
	}
	defer rows.Close()
	var out []Recommendation
	for rows.Next() {
		var r Recommendation
		if err := rows.Scan(&r.WorkspaceID, &r.Difficulty, &r.Model, &r.Provider, &r.ExpectedQuality, &r.Verified, &r.Reason); err != nil {
			return nil, fmt.Errorf("routingbrain: scan recommendation: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const setAutonomousSQL = `INSERT INTO routing_brain_autonomous (workspace_id) VALUES ($1)
ON CONFLICT (workspace_id) DO NOTHING`

// SetAutonomous opts a workspace into autonomous mode (idempotent). Advisory is the
// default; a workspace is autonomous only while present in this table.
func (s *Store) SetAutonomous(ctx context.Context, workspaceID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if _, err := s.db.Exec(ctx, setAutonomousSQL, workspaceID); err != nil {
		return fmt.Errorf("routingbrain: set autonomous: %w", err)
	}
	return nil
}

const clearAutonomousSQL = `DELETE FROM routing_brain_autonomous WHERE workspace_id = $1`

// ClearAutonomous opts a workspace back out to advisory (idempotent).
func (s *Store) ClearAutonomous(ctx context.Context, workspaceID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if _, err := s.db.Exec(ctx, clearAutonomousSQL, workspaceID); err != nil {
		return fmt.Errorf("routingbrain: clear autonomous: %w", err)
	}
	return nil
}

const loadAutonomousSQL = `SELECT workspace_id FROM routing_brain_autonomous`

// LoadAutonomous reads the set of autonomous-opted-in workspaces (for the in-memory
// cache).
func (s *Store) LoadAutonomous(ctx context.Context) ([]string, error) {
	if s == nil || s.rdb == nil {
		return nil, nil
	}
	rows, err := s.rdb.Query(ctx, loadAutonomousSQL)
	if err != nil {
		return nil, fmt.Errorf("routingbrain: load autonomous: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, fmt.Errorf("routingbrain: scan autonomous: %w", err)
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}
