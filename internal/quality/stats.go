// stats.go records per-call quality scores into the
// quality_scores table (migration 0015) and computes the
// workspace-level distribution used by
// `GET /v1/workspaces/:wsID/quality/stats`.
//
// The recorder is best-effort by design — quality scoring sits
// on the proxy hot path, so RecordScore swallows DB errors
// rather than propagating them up. Callers that need an error
// signal can switch to RecordScoreStrict.

package quality

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WorkspaceStats is the response shape for the stats endpoint.
// All percentiles are reported on a 0–1 scale; LowQualityCount
// matches the auto-retry threshold so operators can correlate
// the two metrics.
type WorkspaceStats struct {
	WorkspaceID     string  `json:"workspace_id"`
	WindowDays      int     `json:"window_days"`
	SampleCount     int     `json:"sample_count"`
	AvgScore        float64 `json:"avg_score"`
	P50             float64 `json:"p50"`
	P75             float64 `json:"p75"`
	P95             float64 `json:"p95"`
	LowQualityCount int     `json:"low_quality_count"`
}

const recordScoreSQL = `
INSERT INTO quality_scores (workspace_id, score, provider, model, feature)
VALUES ($1, $2, $3, $4, $5)
`

// RecordScore inserts one observation. workspaceID may be empty
// — when it is we still record the row (with a "(none)" key) so
// the global average remains meaningful.
//
// Best-effort: any DB error logs to the returned error (caller
// can choose to log + discard) and the row is dropped. The
// proxy passes context.Background() with a short timeout so a
// slow DB doesn't slow the hot path.
func (s *Scorer) RecordScore(ctx context.Context, workspaceID, provider, model, feature string, score float64) error {
	if s.pool == nil {
		return nil
	}
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	ws := strings.TrimSpace(workspaceID)
	if ws == "" {
		ws = "(none)"
	}
	if _, err := s.pool.Exec(ctx, recordScoreSQL, ws, score, provider, model, feature); err != nil {
		return fmt.Errorf("quality: record score: %w", err)
	}
	return nil
}

const statsSQL = `
SELECT
    COUNT(*)                                                   AS sample_count,
    COALESCE(AVG(score), 0)                                    AS avg_score,
    COALESCE(PERCENTILE_DISC(0.50) WITHIN GROUP (ORDER BY score), 0) AS p50,
    COALESCE(PERCENTILE_DISC(0.75) WITHIN GROUP (ORDER BY score), 0) AS p75,
    COALESCE(PERCENTILE_DISC(0.95) WITHIN GROUP (ORDER BY score), 0) AS p95,
    COALESCE(SUM(CASE WHEN score < $3 THEN 1 ELSE 0 END), 0)   AS low_quality
FROM quality_scores
WHERE workspace_id = $1 AND created_at >= $2
`

// StatsForWorkspace returns the rolling window for `workspaceID`.
// windowDays=0 falls back to 30. The function is safe to call
// against a freshly migrated database — empty result sets
// produce a zero-valued WorkspaceStats with the correct
// metadata (workspace_id + window_days).
func (s *Scorer) StatsForWorkspace(ctx context.Context, workspaceID string, windowDays int) (*WorkspaceStats, error) {
	if s.pool == nil {
		return nil, errors.New("quality: no database configured")
	}
	if windowDays <= 0 {
		windowDays = 30
	}
	ws := strings.TrimSpace(workspaceID)
	if ws == "" {
		return nil, errors.New("quality: workspace_id required")
	}
	since := time.Now().Add(-time.Duration(windowDays) * 24 * time.Hour)
	row := s.pool.QueryRow(ctx, statsSQL, ws, since, AutoRetryThreshold)

	stats := &WorkspaceStats{WorkspaceID: ws, WindowDays: windowDays}
	if err := row.Scan(
		&stats.SampleCount,
		&stats.AvgScore,
		&stats.P50,
		&stats.P75,
		&stats.P95,
		&stats.LowQualityCount,
	); err != nil {
		return nil, fmt.Errorf("quality: stats query: %w", err)
	}
	return stats, nil
}
