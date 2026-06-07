package poolroyalty

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The Stage-2.2 margin READ surface (COORDINATION "Pool-B economics —
// DECIDED": fee-shaped, MARGIN-IDENTITY ONLY). Talyvor's realized (1−s)
// margin is DERIVED from the claim rows the Stage-2.1 mint already commits
// atomically — margin_usd = avoided_cogs_usd − minted_amount, computed by the
// pool_royalty_margin view (migration 0044) — and read here. Nothing is
// re-recorded, and deliberately nothing is written to token_events: every
// customer spend reader sums token_events.cost_usd with no row-type filter,
// so a margin row there would be miscounted as customer spend. Margin is
// REVENUE; this surface stays separate from every spend surface.

// MarginSummaryRow is the realized Pool-B margin roll-up: how many royalty
// mints happened, the total avoided COGS they represent, the LENS minted to
// contributors (the s side), and Talyvor's realized USD margin (the 1−s side).
type MarginSummaryRow struct {
	Mints          int64
	AvoidedCOGSUSD float64
	MintedLENS     float64
	MarginUSD      float64
}

// MarginByRow is one breakdown bucket (a contributor, a requester, or a layer).
type MarginByRow struct {
	Key string
	MarginSummaryRow
}

// marginDB is the minimal read seam (matches the repo's pgxDB convention) so
// tests inject pgxmock and a nil pool degrades to a no-op.
type marginDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// MarginReader reads the pool_royalty_margin view. The zero/nil reader is
// inert (zero values) — consistent with minting being off by default: no
// mints, no rows, zero margin.
type MarginReader struct {
	db marginDB
}

// NewMarginReader builds a reader over the main pool (or any marginDB).
func NewMarginReader(db marginDB) *MarginReader { return &MarginReader{db: db} }

// REALIZED margin counts FINAL rows only (Stage 2.3a): held rows are
// pending (their minted side may yet be revoked) and revoked rows carry
// fraudulent attribution — counting either overstates realized margin.
const marginSummarySQL = `SELECT COUNT(*),
       COALESCE(SUM(avoided_cogs_usd), 0),
       COALESCE(SUM(minted_amount), 0),
       COALESCE(SUM(margin_usd), 0)
FROM pool_royalty_margin
WHERE status = 'final' AND created_at >= $1`

// marginDimensions is the allow-list for MarginBy. The dimension is
// interpolated into SQL (GROUP BY can't be parameterized), so ONLY these
// exact column names may pass — anything else is rejected before any query.
var marginDimensions = map[string]bool{
	"contributor_workspace_id": true,
	"requester_workspace_id":   true,
	"layer":                    true,
}

// MarginSummary returns realized totals for mints with created_at >= since.
// A zero `since` means all time.
func (r *MarginReader) MarginSummary(ctx context.Context, since time.Time) (MarginSummaryRow, error) {
	if r == nil || r.db == nil {
		return MarginSummaryRow{}, nil
	}
	var s MarginSummaryRow
	if err := r.db.QueryRow(ctx, marginSummarySQL, since).
		Scan(&s.Mints, &s.AvoidedCOGSUSD, &s.MintedLENS, &s.MarginUSD); err != nil {
		return MarginSummaryRow{}, fmt.Errorf("poolroyalty: margin summary: %w", err)
	}
	return s, nil
}

// MarginBy returns the per-bucket breakdown for an allow-listed dimension:
// contributor_workspace_id, requester_workspace_id, or layer.
func (r *MarginReader) MarginBy(ctx context.Context, dimension string, since time.Time) ([]MarginByRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if !marginDimensions[dimension] {
		return nil, fmt.Errorf("poolroyalty: unknown margin dimension %q (allowed: contributor_workspace_id, requester_workspace_id, layer)", dimension)
	}
	// dimension is allow-listed above — safe to interpolate.
	sql := `SELECT ` + dimension + `, COUNT(*),
       COALESCE(SUM(avoided_cogs_usd), 0),
       COALESCE(SUM(minted_amount), 0),
       COALESCE(SUM(margin_usd), 0)
FROM pool_royalty_margin
WHERE status = 'final' AND created_at >= $1
GROUP BY ` + dimension + `
ORDER BY 5 DESC`
	rows, err := r.db.Query(ctx, sql, since)
	if err != nil {
		return nil, fmt.Errorf("poolroyalty: margin by %s: %w", dimension, err)
	}
	defer rows.Close()
	var out []MarginByRow
	for rows.Next() {
		var b MarginByRow
		if err := rows.Scan(&b.Key, &b.Mints, &b.AvoidedCOGSUSD, &b.MintedLENS, &b.MarginUSD); err != nil {
			return nil, fmt.Errorf("poolroyalty: scan margin bucket: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
