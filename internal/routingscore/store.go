// Package routingscore scores routing predictions skill-above-baseline (Proof-of-Improvement piece 3,
// PR-3a). A prediction "cohort C → model M" is proven by running M and the baseline default route on
// cohort C's held eval slice, scoring each vs verifier-held expected_output (eval.StaticScore), and
// recording M's correctness MARGIN over the baseline.
//
// PR-3a builds the FULL mechanism behind an Inferer interface with NO live implementation — it mints
// NOTHING, touches NONE of the serve path (it does NOT import internal/proxy), and reads only held
// eval.StaticScore (never routing_patterns.output_quality — the candidate-C loop). The store is pure
// CRUD + the slice query; the orchestration is in scorer.go.
package routingscore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// store wraps the DB surface the scorer needs.
type store struct{ pool *pgxpool.Pool }

// predictionRow is one active, not-yet-scored prediction the sweeper will score.
type predictionRow struct {
	ID               string
	WorkspaceID      string
	FeatureCategory  string
	InputTokenRange  string
	ComplexityBucket string
	Model            string
}

// scanSQL picks active predictions with NO score row yet (score-once via the LEFT JOIN) — the sweeper's
// per-tick work-list, bounded by BatchLimit at the call site.
const scanSQL = `SELECT p.id, p.workspace_id, p.feature_category, p.input_token_range, p.complexity_bucket, p.model
FROM routing_predictions p
LEFT JOIN routing_prediction_scores s ON s.prediction_id = p.id
WHERE p.status = 'active' AND s.prediction_id IS NULL
LIMIT $1`

func (st *store) activeUnscored(ctx context.Context, limit int) ([]predictionRow, error) {
	rows, err := st.pool.Query(ctx, scanSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("routingscore: scan active unscored: %w", err)
	}
	defer rows.Close()
	var out []predictionRow
	for rows.Next() {
		var p predictionRow
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.FeatureCategory, &p.InputTokenRange, &p.ComplexityBucket, &p.Model); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// sliceSQL selects the held eval slice for a prediction's cohort — items whose three cohort dims match,
// that are tagged (feature_category IS NOT NULL) and active, EXCLUDING items authored by the predictor's
// workspace OR its owner-linkage fingerprint-linked set (the SAME workspace_card_fingerprints self-deal
// join the royalty/eval-contribution paths use). So a predictor is never scored on items they planted.
// Capped at SliceCap at the call site ($5). $1=feature, $2=input_range, $3=complexity, $4=predictor ws.
const sliceSQL = `SELECT e.input, e.expected_output, e.eval_method
FROM benchmark_eval_items e
WHERE e.feature_category IS NOT NULL
  AND e.feature_category = $1 AND e.input_token_range = $2 AND e.complexity_bucket = $3
  AND e.active AND e.status = 'active'
  AND (e.author_workspace_id IS NULL OR (
        e.author_workspace_id <> $4
        AND e.author_workspace_id NOT IN (
            SELECT b.workspace_id FROM workspace_card_fingerprints a
            JOIN workspace_card_fingerprints b ON a.fingerprint_hash = b.fingerprint_hash
            WHERE a.workspace_id = $4)))
LIMIT $5`

// sliceItem is one held item to run M + baseline on.
type sliceItem struct {
	Input          string
	ExpectedOutput string
	EvalMethod     string
}

func (st *store) cohortSlice(ctx context.Context, feature, inputRange, complexity, predictorWS string, cap int) ([]sliceItem, error) {
	rows, err := st.pool.Query(ctx, sliceSQL, feature, inputRange, complexity, predictorWS, cap)
	if err != nil {
		return nil, fmt.Errorf("routingscore: cohort slice: %w", err)
	}
	defer rows.Close()
	var out []sliceItem
	for rows.Next() {
		var it sliceItem
		if err := rows.Scan(&it.Input, &it.ExpectedOutput, &it.EvalMethod); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// insertScore records the skill-above-baseline result. prediction_id is UNIQUE (score-once); a conflict
// (a concurrent/duplicate sweep) is a no-op.
const insertScoreSQL = `INSERT INTO routing_prediction_scores
    (prediction_id, slice_size, m_avg, baseline_avg, baseline_model, skill_margin)
VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (prediction_id) DO NOTHING`

func (st *store) insertScore(ctx context.Context, predictionID string, sliceSize int, mAvg, baselineAvg float64, baselineModel string, skill float64) error {
	_, err := st.pool.Exec(ctx, insertScoreSQL, predictionID, sliceSize, mAvg, baselineAvg, baselineModel, skill)
	if err != nil {
		return fmt.Errorf("routingscore: insert score: %w", err)
	}
	return nil
}
