// Package routedecision is the DESCRIPTIVE, MINT-FREE evidence store for the routing Advisor's cost impact.
// For each auto-routed request it records the pre-cohort baseline model, the served model, whether
// cross-tenant cohort intelligence overrode the baseline, and the served vs. counterfactual cost — the
// go/no-go data for whether a corpus-contribution mint (KE-1) is worth building.
//
// It MOVES NO MONEY: it imports no ledger/economy package and exposes only Exec/QueryRow seams. The
// counterfactual cost is an ESTIMATE (a different model emits different tokens), stored as such and never as a
// "saving" — this table is evidence, not value. See migration 0092.
package routedecision

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RouteDecision is one auto-routed request's descriptive record. Costs are integer µ-USD (SEC-2 discipline).
type RouteDecision struct {
	WorkspaceID                 string // SELF only
	BaselineModel               string
	ActualModel                 string
	CohortOverrode              bool
	CohortBasis                 string
	CohortN                     int
	InputTokens                 int
	OutputTokens                int
	ActualCostU                 int64
	CounterfactualCostEstimateU int64 // ⚠ ESTIMATE, not money
}

// writeDB is the Exec-only write seam (*pgxpool.Pool satisfies it). No mint surface.
type writeDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Writer persists route-decision evidence.
type Writer struct{ db writeDB }

func NewWriter(db writeDB) *Writer { return &Writer{db: db} }

const insertSQL = `INSERT INTO routing_decisions
    (workspace_id, baseline_model, actual_model, cohort_overrode, cohort_basis, cohort_n,
     input_tokens, output_tokens, actual_cost_u, counterfactual_cost_estimate_u)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

// Record writes one route-decision row. Best-effort evidence; the caller runs it off the serve path.
func (w *Writer) Record(ctx context.Context, r RouteDecision) error {
	if w == nil || w.db == nil {
		return nil
	}
	_, err := w.db.Exec(ctx, insertSQL,
		r.WorkspaceID, r.BaselineModel, r.ActualModel, r.CohortOverrode, r.CohortBasis, r.CohortN,
		r.InputTokens, r.OutputTokens, r.ActualCostU, r.CounterfactualCostEstimateU)
	if err != nil {
		return fmt.Errorf("routedecision: record: %w", err)
	}
	return nil
}

// readDB is the QueryRow-only read seam.
type readDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Summary is the go/no-go readout over a window: how often the cohort overrode the baseline, and the
// AGGREGATE ESTIMATED cost delta (counterfactual − actual). EstimatedCostDeltaU is an ESTIMATE of savings,
// NOT money — a mint would pay strictly less, floored at zero.
type Summary struct {
	TotalRequests                int64
	OverrideCount                int64
	OverrideRate                 float64
	TotalActualCostU             int64
	TotalCounterfactualEstimateU int64
	EstimatedCostDeltaU          int64 // ⚠ counterfactual − actual, ESTIMATE only
}

// Reader answers the go/no-go summary.
type Reader struct{ db readDB }

func NewReader(db readDB) *Reader { return &Reader{db: db} }

const summarySQL = `SELECT
    COUNT(*),
    COUNT(*) FILTER (WHERE cohort_overrode),
    COALESCE(SUM(actual_cost_u), 0),
    COALESCE(SUM(counterfactual_cost_estimate_u), 0)
FROM routing_decisions WHERE created_at >= $1`

// Summarize computes the window readout. OverrideRate is override/total (0 when no requests).
func (r *Reader) Summarize(ctx context.Context, since time.Time) (Summary, error) {
	var s Summary
	if err := r.db.QueryRow(ctx, summarySQL, since).
		Scan(&s.TotalRequests, &s.OverrideCount, &s.TotalActualCostU, &s.TotalCounterfactualEstimateU); err != nil {
		return Summary{}, fmt.Errorf("routedecision: summarize: %w", err)
	}
	if s.TotalRequests > 0 {
		s.OverrideRate = float64(s.OverrideCount) / float64(s.TotalRequests)
	}
	s.EstimatedCostDeltaU = s.TotalCounterfactualEstimateU - s.TotalActualCostU
	return s, nil
}
