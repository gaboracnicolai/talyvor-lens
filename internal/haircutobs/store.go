// Package haircutobs is the READ-ONLY observability surface for KE-2: it surfaces every APPLIED drift haircut
// so an operator can see "is the guard firing, on whom, and how hard" — the point of running it default-on in
// closed-test. It reads the haircut factor already stamped into the mint's ledger metadata
// (drift_haircut_factor) and LEFT-JOINs the workspace's causing hardened finding. It MOVES NO MONEY (read-only;
// no ledger/mint constructor).
package haircutobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// HaircutEvent is one applied haircut: the mint it reduced + the finding that caused it. µLENS are integers.
type HaircutEvent struct {
	WorkspaceID    string    `json:"workspace_id"`
	Factor         float64   `json:"factor"`          // drift_haircut_factor (the haircut's own multiplier)
	BaseULens      int64     `json:"base_ulens"`      // pre-scaling mint base
	EffectiveULens int64     `json:"effective_ulens"` // post reputation×haircut mint
	MintType       string    `json:"mint_type"`       // the bonded ledger type (pool_royalty_held / receipt_mine_provisional)
	CreatedAt      time.Time `json:"created_at"`
	DeviationSigma *float64  `json:"deviation_sigma"` // the causing hardened finding's severity (nil if none current)
	Unit           *string   `json:"unit"`            // the finding's comparison unit (provider/model)
	WindowBucket   *int64    `json:"window_bucket"`
}

// readDB is the QueryRow/Query seam.
type readDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Reader answers the recent-haircuts query.
type Reader struct{ db readDB }

func NewReader(db readDB) *Reader { return &Reader{db: db} }

// recentSQL: applied haircuts (ledger rows carrying drift_haircut_factor) in the window, each joined to the
// workspace's most-recent hardened idiosyncratic finding (the cause). Capped at 500 rows.
const recentSQL = `SELECT
    l.workspace_id,
    (l.metadata->>'drift_haircut_factor')::float8,
    COALESCE((l.metadata->>'reputation_base_ulens')::bigint, 0),
    COALESCE((l.metadata->>'reputation_effective_ulens')::bigint, 0),
    l.type,
    l.created_at,
    f.deviation_sigma,
    f.unit,
    f.window_bucket
FROM lens_token_ledger l
LEFT JOIN LATERAL (
    SELECT deviation_sigma, unit, window_bucket
    FROM keel_findings
    WHERE workspace_id = l.workspace_id AND mode = 'hardened' AND attribution = 'idiosyncratic'
    ORDER BY first_seen_at DESC
    LIMIT 1
) f ON TRUE
WHERE l.metadata ? 'drift_haircut_factor' AND l.created_at >= $1
ORDER BY l.created_at DESC
LIMIT 500`

// Recent returns applied haircuts since `since` (most recent first).
func (r *Reader) Recent(ctx context.Context, since time.Time) ([]HaircutEvent, error) {
	rows, err := r.db.Query(ctx, recentSQL, since)
	if err != nil {
		return nil, fmt.Errorf("haircutobs: recent: %w", err)
	}
	defer rows.Close()
	var out []HaircutEvent
	for rows.Next() {
		var e HaircutEvent
		if err := rows.Scan(&e.WorkspaceID, &e.Factor, &e.BaseULens, &e.EffectiveULens, &e.MintType,
			&e.CreatedAt, &e.DeviationSigma, &e.Unit, &e.WindowBucket); err != nil {
			return nil, fmt.Errorf("haircutobs: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("haircutobs: rows: %w", err)
	}
	return out, nil
}
