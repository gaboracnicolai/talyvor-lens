package alerts

import (
	"context"
	"fmt"
)

// TareMeter is one Tare reduction (internal/tare), recorded on the request's OWN token_events row —
// the billing write that already meters every upstream call. B6.4. The zero value means Tare did not
// change the request, and writes the column defaults.
//
// ⚠ WorkItemID IS CALLER-DECLARED (X-Talyvor-Issue, the same string request_attribution.issue_id
// holds). It labels a customer's own saving for display; it is never an economy input. See
// migrations/0126_tare_metering.sql and docs/tare-phase1d-brief.md.
type TareMeter struct {
	Kind       string // the tare.Kind whose reducer shrank the request; "" = not reduced
	TokensIn   int    // request-body token estimate before the reduction
	TokensOut  int    // and after it
	WorkItemID string
}

// deltaCostUSD prices the tokens Tare removed at the billed model's INPUT rate — the same price
// table cost_usd on the same row uses, so the saving and the charge can never disagree on a rate.
func (t TareMeter) deltaCostUSD(model string) float64 {
	if t.Kind == "" || t.TokensIn <= t.TokensOut {
		return 0
	}
	return costUSD(model, t.TokensIn-t.TokensOut, 0)
}

// TareSaving is one work item's Tare savings in a workspace. WorkItemID "" collects the reduced
// requests that declared no work item.
type TareSaving struct {
	WorkItemID   string  `json:"work_item_id"`
	Requests     int64   `json:"requests"`
	TokensIn     int64   `json:"tokens_in"`
	TokensOut    int64   `json:"tokens_out"`
	DeltaCostUSD float64 `json:"delta_cost_usd"`
}

const tareSavingsSQL = `SELECT tare_work_item_id, COUNT(*), SUM(tare_tokens_in), SUM(tare_tokens_out), SUM(tare_delta_cost_usd)
FROM token_events
WHERE workspace_id = $1 AND tare_kind <> ''
GROUP BY tare_work_item_id
ORDER BY SUM(tare_delta_cost_usd) DESC, tare_work_item_id`

// TareSavings is the reader for the tare_* columns: what Tare saved in this workspace, per work item,
// largest saving first.
func (a *AlertManager) TareSavings(ctx context.Context, workspaceID string) ([]TareSaving, error) {
	rows, err := a.pool.Query(ctx, tareSavingsSQL, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("alerts: tare savings: %w", err)
	}
	defer rows.Close()
	out := []TareSaving{}
	for rows.Next() {
		var s TareSaving
		if err := rows.Scan(&s.WorkItemID, &s.Requests, &s.TokensIn, &s.TokensOut, &s.DeltaCostUSD); err != nil {
			return nil, fmt.Errorf("alerts: tare savings scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
