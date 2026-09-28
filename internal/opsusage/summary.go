// Package opsusage is the operator's usage and spend readout across every workspace (B17.7): requests,
// tokens, provider cost and who served each answer (model, own cache, shared pool) over a window, plus
// how many workspaces there are and how many had traffic.
//
// Synthetic workspaces (B17.1) are counted apart: the default audience leaves them out, so a harness run
// never moves a number an operator reads, and the synthetic audience shows the harness its own traffic.
//
// It MOVES NO MONEY: token_events.cost_usd is what the provider cost Talyvor, a descriptive figure. The
// reader holds a Query-only seam.
package opsusage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/workspace"
)

type readDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Summary is one audience's totals over a window.
type Summary struct {
	Audience       string           `json:"audience"`
	Workspaces     int64            `json:"workspaces"`              // active workspaces in the audience
	WithTraffic    int64            `json:"workspaces_with_traffic"` // of those, how many made a request in the window
	Requests       int64            `json:"requests"`                // token_events rows in the window
	InputTokens    int64            `json:"input_tokens"`
	OutputTokens   int64            `json:"output_tokens"`
	CostUSD        float64          `json:"provider_cost_usd"` // what the providers cost Talyvor; not a charge
	ServesBySource map[string]int64 `json:"serves_by_source"`  // upstream, cache_hit_exact, cache_hit_pooled, …
}

// Reader answers Summarize.
type Reader struct{ db readDB }

func NewReader(db readDB) *Reader { return &Reader{db: db} }

const (
	workspacesSQL = `SELECT count(*) FROM workspaces WHERE active AND synthetic = $1`

	totalsSQL = `SELECT count(*), count(DISTINCT te.workspace_id),
    COALESCE(sum(te.input_tokens), 0), COALESCE(sum(te.output_tokens), 0), COALESCE(sum(te.cost_usd), 0)
FROM token_events te WHERE te.created_at >= $1 AND `

	bySourceSQL = `SELECT te.serve_source, count(*) FROM token_events te WHERE te.created_at >= $1 AND %s
GROUP BY te.serve_source ORDER BY te.serve_source`
)

// Summarize reads audience's totals for traffic since since.
func (r *Reader) Summarize(ctx context.Context, audience workspace.Audience, since time.Time) (Summary, error) {
	s := Summary{Audience: audience.String(), ServesBySource: map[string]int64{}}
	if r == nil || r.db == nil {
		return s, fmt.Errorf("opsusage: no database")
	}
	if err := r.db.QueryRow(ctx, workspacesSQL, audience == workspace.AudienceSynthetic).Scan(&s.Workspaces); err != nil {
		return s, fmt.Errorf("opsusage: workspaces: %w", err)
	}
	pred := audience.SQL("te.workspace_id")
	if err := r.db.QueryRow(ctx, totalsSQL+pred, since).Scan(
		&s.Requests, &s.WithTraffic, &s.InputTokens, &s.OutputTokens, &s.CostUSD); err != nil {
		return s, fmt.Errorf("opsusage: totals: %w", err)
	}
	rows, err := r.db.Query(ctx, fmt.Sprintf(bySourceSQL, pred), since)
	if err != nil {
		return s, fmt.Errorf("opsusage: serves by source: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		var n int64
		if err := rows.Scan(&src, &n); err != nil {
			return s, fmt.Errorf("opsusage: scan source: %w", err)
		}
		s.ServesBySource[src] = n
	}
	return s, rows.Err()
}
