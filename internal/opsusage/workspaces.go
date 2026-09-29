package opsusage

import (
	"context"
	"fmt"
	"time"

	"github.com/talyvor/lens/internal/workspace"
)

// B18.17 — the operator screen's per-workspace columns (old queue W1.4): each workspace's spend, the LENS
// it holds unsettled, and when it last made a request. Every workspace in the audience has a row, traffic
// or not. Query-only, like Summarize: nothing here writes a row or moves money.

// WorkspaceSpend is what one workspace's requests cost: SUM(token_events.cost_usd), the figure the
// workspace's own /spend/current-month reads.
type WorkspaceSpend struct {
	WorkspaceID     string  `json:"workspace_id"`
	CurrentMonthUSD float64 `json:"current_month_usd"`
	AllTimeUSD      float64 `json:"all_time_usd"`
	Requests        int64   `json:"requests"`
}

// WorkspaceHeld is the LENS a workspace has earned and not yet settled (lens_token_balances.held_balance,
// in µLENS) — still in its holdback, still revocable.
type WorkspaceHeld struct {
	WorkspaceID string `json:"workspace_id"`
	HeldULENS   int64  `json:"held_ulens"`
}

// WorkspaceActivity is when a workspace last made a request; nil when it never has.
type WorkspaceActivity struct {
	WorkspaceID   string     `json:"workspace_id"`
	LastRequestAt *time.Time `json:"last_request_at"`
}

const (
	spendByWorkspaceSQL = `SELECT w.id,
    COALESCE(sum(te.cost_usd) FILTER (WHERE te.created_at >= date_trunc('month', now())), 0),
    COALESCE(sum(te.cost_usd), 0), count(te.workspace_id)
FROM workspaces w LEFT JOIN token_events te ON te.workspace_id = w.id
WHERE w.synthetic = $1 GROUP BY w.id ORDER BY w.id`

	heldByWorkspaceSQL = `SELECT w.id, COALESCE(b.held_balance, 0)::bigint
FROM workspaces w LEFT JOIN lens_token_balances b ON b.workspace_id = w.id
WHERE w.synthetic = $1 ORDER BY w.id`

	activityByWorkspaceSQL = `SELECT w.id, max(te.created_at)
FROM workspaces w LEFT JOIN token_events te ON te.workspace_id = w.id
WHERE w.synthetic = $1 GROUP BY w.id ORDER BY w.id`
)

// SpendByWorkspace is every workspace's spend in the audience.
func (r *Reader) SpendByWorkspace(ctx context.Context, audience workspace.Audience) ([]WorkspaceSpend, error) {
	return collect(r, ctx, spendByWorkspaceSQL, audience, func(scan func(...any) error) (WorkspaceSpend, error) {
		var s WorkspaceSpend
		return s, scan(&s.WorkspaceID, &s.CurrentMonthUSD, &s.AllTimeUSD, &s.Requests)
	})
}

// HeldByWorkspace is every workspace's unsettled LENS in the audience.
func (r *Reader) HeldByWorkspace(ctx context.Context, audience workspace.Audience) ([]WorkspaceHeld, error) {
	return collect(r, ctx, heldByWorkspaceSQL, audience, func(scan func(...any) error) (WorkspaceHeld, error) {
		var h WorkspaceHeld
		return h, scan(&h.WorkspaceID, &h.HeldULENS)
	})
}

// ActivityByWorkspace is every workspace's last request in the audience.
func (r *Reader) ActivityByWorkspace(ctx context.Context, audience workspace.Audience) ([]WorkspaceActivity, error) {
	return collect(r, ctx, activityByWorkspaceSQL, audience, func(scan func(...any) error) (WorkspaceActivity, error) {
		var a WorkspaceActivity
		return a, scan(&a.WorkspaceID, &a.LastRequestAt)
	})
}

func collect[T any](r *Reader, ctx context.Context, sql string, audience workspace.Audience, row func(func(...any) error) (T, error)) ([]T, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("opsusage: no database")
	}
	rows, err := r.db.Query(ctx, sql, audience == workspace.AudienceSynthetic)
	if err != nil {
		return nil, fmt.Errorf("opsusage: per-workspace read: %w", err)
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := row(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("opsusage: scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
