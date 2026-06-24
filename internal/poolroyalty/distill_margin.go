// distill_margin.go (PR4) — the distill reuse-royalty MARGIN read surface, mirroring
// the cache pool_royalty_margin (margin.go / migration 0044). Talyvor's realized (1−s)
// margin on a distill OCR reuse is the same identity — margin_usd = avoided_cogs_usd −
// minted_amount — DERIVED by the distill_royalty_margin view (migration 0064) and read
// here. Nothing is re-recorded; deliberately NOT a token_events write (margin is REVENUE,
// not customer COST — same reasoning as the cache margin). REALIZED margin counts FINAL
// rows only: held rows may yet be revoked, and revoked rows carry fraudulent attribution
// (PR3) — counting either overstates realized margin. Inert by default: minting off → empty
// table → empty view → zero margin.
//
// Reuses MarginSummaryRow / MarginByRow / marginDB from margin.go; only the view name and
// the breakdown dimensions differ (distill has NO layer — its third dimension is content_hash).
package poolroyalty

import (
	"context"
	"fmt"
	"time"
)

// DistillMarginReader reads the distill_royalty_margin view. The zero/nil reader is inert.
type DistillMarginReader struct {
	db marginDB
}

// NewDistillMarginReader builds a reader over the main pool (or any marginDB).
func NewDistillMarginReader(db marginDB) *DistillMarginReader { return &DistillMarginReader{db: db} }

// REALIZED margin counts FINAL rows only (held may be revoked; revoked carries
// fraudulent attribution).
const distillMarginSummarySQL = `SELECT COUNT(*),
       COALESCE(SUM(avoided_cogs_usd), 0),
       COALESCE(SUM(minted_amount), 0),
       COALESCE(SUM(margin_usd), 0)
FROM distill_royalty_margin
WHERE status = 'final' AND created_at >= $1`

// distillMarginDimensions is the allow-list for DistillMarginBy. The dimension is
// interpolated into SQL (GROUP BY can't be parameterized), so ONLY these exact column
// names may pass. Distill has NO layer; its document dimension is content_hash.
var distillMarginDimensions = map[string]bool{
	"contributor_workspace_id": true,
	"requester_workspace_id":   true,
	"content_hash":             true,
}

// MarginSummary returns realized totals for distill mints with created_at >= since.
// A zero `since` means all time.
func (r *DistillMarginReader) MarginSummary(ctx context.Context, since time.Time) (MarginSummaryRow, error) {
	if r == nil || r.db == nil {
		return MarginSummaryRow{}, nil
	}
	var s MarginSummaryRow
	if err := r.db.QueryRow(ctx, distillMarginSummarySQL, since).
		Scan(&s.Mints, &s.AvoidedCOGSUSD, &s.MintedLENS, &s.MarginUSD); err != nil {
		return MarginSummaryRow{}, fmt.Errorf("poolroyalty: distill margin summary: %w", err)
	}
	return s, nil
}

// MarginBy returns the per-bucket breakdown for an allow-listed dimension:
// contributor_workspace_id, requester_workspace_id, or content_hash.
func (r *DistillMarginReader) MarginBy(ctx context.Context, dimension string, since time.Time) ([]MarginByRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if !distillMarginDimensions[dimension] {
		return nil, fmt.Errorf("poolroyalty: unknown distill margin dimension %q (allowed: contributor_workspace_id, requester_workspace_id, content_hash)", dimension)
	}
	// dimension is allow-listed above — safe to interpolate.
	sql := `SELECT ` + dimension + `, COUNT(*),
       COALESCE(SUM(avoided_cogs_usd), 0),
       COALESCE(SUM(minted_amount), 0),
       COALESCE(SUM(margin_usd), 0)
FROM distill_royalty_margin
WHERE status = 'final' AND created_at >= $1
GROUP BY ` + dimension + `
ORDER BY 5 DESC`
	rows, err := r.db.Query(ctx, sql, since)
	if err != nil {
		return nil, fmt.Errorf("poolroyalty: distill margin by %s: %w", dimension, err)
	}
	defer rows.Close()
	var out []MarginByRow
	for rows.Next() {
		var b MarginByRow
		if err := rows.Scan(&b.Key, &b.Mints, &b.AvoidedCOGSUSD, &b.MintedLENS, &b.MarginUSD); err != nil {
			return nil, fmt.Errorf("poolroyalty: scan distill margin bucket: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
