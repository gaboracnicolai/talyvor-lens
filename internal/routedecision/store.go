// Package routedecision is the DESCRIPTIVE, MINT-FREE evidence store for the routing Advisor's cost impact.
// For each auto-routed request it records the pre-cohort baseline model, the served model, whether
// cross-tenant cohort intelligence overrode the baseline, and the served vs. counterfactual cost — the
// go/no-go data for whether a corpus-contribution mint (KE-1) is worth building.
//
// It MOVES NO MONEY: it imports no ledger/economy package and exposes only Exec/QueryRow seams. The
// counterfactual cost is an ESTIMATE (a different model emits different tokens), stored as such and never as a
// "saving" — this table is evidence, not value. See migration 0092.
//
// ⚠ BEFORE YOU QUOTE A PERCENTAGE FROM THIS TABLE, read the limits on the Summary type. In short: the
// denominator is auto-routed requests only (NOT all traffic), the counterfactual reuses the served model's
// token counts, and the sample is shed under load. Those are structural, not bugs — a figure derived from this
// substrate has to state them or it repeats the unmeasured-claim problem it exists to fix.
//
// Costs are priced CACHE-AWARE (alerts.CostUSDDetailed) as of migration 0107. Rows written before that are
// labelled BasisFlat and CANNOT be repriced — the cached/uncached split was never stored — so the aggregate
// excludes them and reports the count instead of averaging across a basis change.
package routedecision

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Cost-basis labels. A row says which pricing basis produced its two cost figures, so a reader can never
// average across the changeover without noticing — see the package header.
const (
	// BasisCacheAware: priced with alerts.CostUSDDetailed — cached input at the provider's cache-read rate,
	// cache writes at the write rate. The honest basis, and the only one worth aggregating.
	BasisCacheAware = "cache_aware"
	// BasisFlat: priced with alerts.CostUSD — EVERY input token at the full input rate, blind to prompt
	// caching. Every row captured before the cache-aware fix, plus any row where the provider reported no
	// usage breakdown (there is then nothing to split, so flat is the only honest basis available).
	BasisFlat = "flat"
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
	// CostBasis is BasisCacheAware or BasisFlat — which pricing produced the two figures above. An empty
	// value is written as BasisFlat: rows predate the label only because they predate the fix.
	CostBasis string
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
     input_tokens, output_tokens, actual_cost_u, counterfactual_cost_estimate_u, cost_basis)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`

// Record writes one route-decision row. Best-effort evidence; the caller runs it off the serve path.
func (w *Writer) Record(ctx context.Context, r RouteDecision) error {
	if w == nil || w.db == nil {
		return nil
	}
	basis := r.CostBasis
	if basis == "" {
		basis = BasisFlat // an unlabelled row is a flat-priced row; never let it pass as measured
	}
	_, err := w.db.Exec(ctx, insertSQL,
		r.WorkspaceID, r.BaselineModel, r.ActualModel, r.CohortOverrode, r.CohortBasis, r.CohortN,
		r.InputTokens, r.OutputTokens, r.ActualCostU, r.CounterfactualCostEstimateU, basis)
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
//
// ⚠ READ THIS BEFORE QUOTING A PERCENTAGE FROM IT. The denominator is NOT "all traffic", and three limits
// are structural, not bugs:
//
//  1. THE COUNTERFACTUAL REUSES THE SERVED MODEL'S TOKEN COUNTS. The baseline call never happened, so its
//     output length is assumed equal to what the served model actually emitted. A different model emits a
//     different number of tokens (cheaper models are often chattier), so the baseline figure is modelled,
//     not measured. Only a holdout — routing real traffic to the baseline and comparing settled spend —
//     turns this into an observation.
//  2. AUTO-ROUTED REQUESTS ONLY. A row exists only when the Advisor had a known pre-cohort baseline to
//     compare against. Requests that were never auto-routed contribute nothing, so TotalRequests is a
//     SUBSET of the workspace's traffic. A ratio computed from this describes routed traffic; saying
//     "we cut costs N%" from it would silently widen the denominator to traffic never in the sample.
//  3. THE SAMPLE IS SHED UNDER LOAD. Capture shares the observational writer bound and is dropped when it
//     is exhausted (drops are logged, never silent in the log — but they ARE absent from this table). The
//     sample therefore under-represents peak load, which is when routing decisions matter most.
//
// A fourth, softer limit: the cached/uncached split comes from the provider's reported usage. Where the
// provider reports none, the row is BasisFlat and is excluded here (see ExcludedLegacyBasisRows).
type Summary struct {
	TotalRequests                int64
	OverrideCount                int64
	OverrideRate                 float64
	TotalActualCostU             int64
	TotalCounterfactualEstimateU int64
	EstimatedCostDeltaU          int64 // ⚠ counterfactual − actual, ESTIMATE only
	// ExcludedLegacyBasisRows counts in-window rows on the retired FLAT basis that were deliberately NOT
	// summed above. They cannot be repriced — the cached/uncached split they would need was never stored —
	// so mixing them in would average across a basis change. Non-zero means: history exists, it is not in
	// these numbers, and it is not recoverable. Report it; do not quietly add it back.
	ExcludedLegacyBasisRows int64
}

// Reader answers the go/no-go summary.
type Reader struct{ db readDB }

func NewReader(db readDB) *Reader { return &Reader{db: db} }

// The aggregate sums ONLY cache-aware rows, and counts the flat ones separately. Both queries share the
// same window predicate so the exclusion count describes exactly the window being reported.
const (
	summaryScopedSQL = `SELECT
    COUNT(*) FILTER (WHERE cost_basis = 'cache_aware'),
    COUNT(*) FILTER (WHERE cost_basis = 'cache_aware' AND cohort_overrode),
    COALESCE(SUM(actual_cost_u) FILTER (WHERE cost_basis = 'cache_aware'), 0),
    COALESCE(SUM(counterfactual_cost_estimate_u) FILTER (WHERE cost_basis = 'cache_aware'), 0),
    COUNT(*) FILTER (WHERE cost_basis <> 'cache_aware')
FROM routing_decisions WHERE workspace_id = $1 AND created_at >= $2`

	summaryAllTenantsSQL = `SELECT
    COUNT(*) FILTER (WHERE cost_basis = 'cache_aware'),
    COUNT(*) FILTER (WHERE cost_basis = 'cache_aware' AND cohort_overrode),
    COALESCE(SUM(actual_cost_u) FILTER (WHERE cost_basis = 'cache_aware'), 0),
    COALESCE(SUM(counterfactual_cost_estimate_u) FILTER (WHERE cost_basis = 'cache_aware'), 0),
    COUNT(*) FILTER (WHERE cost_basis <> 'cache_aware')
FROM routing_decisions WHERE created_at >= $1`
)

// Summarize computes ONE WORKSPACE'S window readout — the scoped read, and the one anything customer-facing
// must use.
//
// ⚠ WHY THIS TAKES A WORKSPACE. It used to aggregate every tenant's rows with no filter. Nothing customer-
// facing consumed it, which is precisely why it was worth fixing before something did: an unscoped summary
// is the shape a "show the customer their own saving" surface would proxy, and proxying it would hand one
// tenant another tenant's spend. The predicate rides idx_routing_decisions_ws (workspace_id, created_at
// DESC), which migration 0092 already created — scoping cost one predicate and no migration.
//
// An EMPTY workspaceID matches nothing (it is not a wildcard): a caller that forgot to pass an id gets an
// empty summary, never everyone's.
func (r *Reader) Summarize(ctx context.Context, workspaceID string, since time.Time) (Summary, error) {
	return r.summarize(ctx, summaryScopedSQL, workspaceID, since)
}

// SummarizeAllTenants is the CROSS-TENANT forensic read, for the admin endpoint only. It is named this way
// so it cannot be reached by accident: any caller aggregating every tenant's spend has to say so out loud.
// Never serve its result to a customer.
func (r *Reader) SummarizeAllTenants(ctx context.Context, since time.Time) (Summary, error) {
	return r.summarize(ctx, summaryAllTenantsSQL, since)
}

func (r *Reader) summarize(ctx context.Context, query string, args ...any) (Summary, error) {
	var s Summary
	if err := r.db.QueryRow(ctx, query, args...).Scan(
		&s.TotalRequests, &s.OverrideCount, &s.TotalActualCostU, &s.TotalCounterfactualEstimateU,
		&s.ExcludedLegacyBasisRows); err != nil {
		return Summary{}, fmt.Errorf("routedecision: summarize: %w", err)
	}
	if s.TotalRequests > 0 {
		s.OverrideRate = float64(s.OverrideCount) / float64(s.TotalRequests)
	}
	s.EstimatedCostDeltaU = s.TotalCounterfactualEstimateU - s.TotalActualCostU
	return s, nil
}
