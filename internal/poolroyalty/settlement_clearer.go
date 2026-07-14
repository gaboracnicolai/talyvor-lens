// settlement_clearer.go — Phase-3 Item 3 (the clearer half of settlement fail-
// closed). The fail-closed FinalizeSweeper settles ONLY status='cleared' rows;
// this is the ONLY thing that promotes held→cleared, and it does so exclusively
// for rows the ring detector EXAMINED this tick and did NOT flag, and that are
// already DUE. So:
//   - a row the detector never examined (detector down/lagging) is never cleared
//     → the sweeper never settles it → fail-closed (the money guarantee).
//   - a flagged (self-dealing ring) row is never cleared → never settles (the
//     AutoAdjudicator additionally BURNS it; this clearer just refuses to clear it).
//   - a not-yet-due row is not cleared, so the detector keeps the FULL holdback
//     window to catch a late-appearing identity link before the row can settle.
//
// FAIL-CLOSED on ambiguity: a detector error clears nothing (rows stay held,
// retried next tick). DEFAULT OFF: a nil/false enable gate is a total no-op.
package poolroyalty

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// partitionDetector is the read-only detect seam — *RingDetector satisfies it.
type partitionDetector interface {
	DetectAndPartition(ctx context.Context, window time.Duration) (examined []string, flags []RingFlag, err error)
}

// clearerDB is the minimal mutate seam for the clear CAS (*pgxpool.Pool satisfies it).
type clearerDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SettlementClearer promotes examined-clean-and-due held rows to 'cleared'.
type SettlementClearer struct {
	detector partitionDetector
	db       clearerDB
	table    string
	enabled  func() bool
	window   time.Duration
}

func NewSettlementClearer(detector partitionDetector, db clearerDB, table string, enabled func() bool, window time.Duration) *SettlementClearer {
	if window <= 0 {
		window = 24 * time.Hour
	}
	if table == "" {
		table = "pool_royalty_mints"
	}
	return &SettlementClearer{detector: detector, db: db, table: table, enabled: enabled, window: window}
}

// clearCASSQLFor promotes examined-clean-DUE held rows to 'cleared'. Both `table`
// and the status literals are TRUSTED internal constants (never user input), so
// the fmt.Sprintf is injection-safe (mirrors sweeper/revoker). The status='held'
// guard makes it exactly-once vs a concurrent revoke/finalize; finalize_after <
// now() keeps the full window for detection.
func clearCASSQLFor(table string) string {
	return fmt.Sprintf(`UPDATE %s SET status = 'cleared'
WHERE request_id = ANY($1) AND status = 'held' AND finalize_after < now()`, table)
}

// RunOnce promotes examined-clean-and-due held rows to 'cleared' and returns the
// count cleared. Inert when disabled; FAIL-CLOSED on a detector error (clears
// nothing, retried next tick).
func (c *SettlementClearer) RunOnce(ctx context.Context) (int, error) {
	if c == nil || c.detector == nil || c.db == nil || c.enabled == nil || !c.enabled() {
		return 0, nil // DEFAULT OFF: total no-op
	}
	examined, flags, err := c.detector.DetectAndPartition(ctx, c.window)
	if err != nil {
		return 0, err // FAIL-CLOSED: never clear on an unknown/partial picture
	}
	if len(examined) == 0 {
		return 0, nil
	}
	flagged := make(map[string]struct{}, len(flags))
	for _, f := range flags {
		flagged[f.RequestID] = struct{}{}
	}
	clearIDs := make([]string, 0, len(examined))
	for _, id := range examined {
		if _, bad := flagged[id]; !bad {
			clearIDs = append(clearIDs, id) // examined − flagged
		}
	}
	if len(clearIDs) == 0 {
		return 0, nil
	}
	tag, err := c.db.Exec(ctx, clearCASSQLFor(c.table), clearIDs)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// StartScheduler ticks RunOnce until ctx ends — mirrors the finalize/auto-adjudicate
// sweepers. Leader-elected by the caller. Inert while disabled; fail-closed on error.
func (c *SettlementClearer) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := c.RunOnce(ctx); err != nil {
				slog.Warn("settlement clearer: sweep failed (fail-closed; nothing cleared this tick)",
					slog.String("table", c.table), slog.String("error", err.Error()))
			}
		}
	}
}
