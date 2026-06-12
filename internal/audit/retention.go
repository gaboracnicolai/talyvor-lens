package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// retentionTable is HARDCODED. There is deliberately NO table knob: the supply
// ledgers (lens_token_ledger, lxc_ledger) must NEVER be retention-swept
// (GetTotalSupply sums their full history), so the only deletable audit table is
// token_events. A config can never point this sweeper at a ledger.
const retentionTable = "token_events"

// retentionBatch bounds the rows deleted per transaction, so a large purge on the
// highest-volume table can't take a long lock or bloat WAL/vacuum in one shot.
const retentionBatch = 5000

// Retention is the token_events-only retention sweeper. It is the ONLY sanctioned
// deleter of audit rows: each batch runs `SET LOCAL lens.audit_retention = 'on'`
// so the 0055 trigger permits the DELETE (a stray DELETE without the flag is still
// blocked). UPDATE/TRUNCATE remain blocked even here.
type Retention struct {
	pool   *pgxpool.Pool
	window time.Duration
	batch  int
	log    *slog.Logger
}

// NewRetention builds the sweeper. window <= 0 disables it.
func NewRetention(pool *pgxpool.Pool, window time.Duration) *Retention {
	return &Retention{pool: pool, window: window, batch: retentionBatch, log: slog.Default()}
}

// SweepOnce deletes token_events rows older than the window in batches, returning
// the total deleted. window <= 0 is a no-op (disabled).
func (r *Retention) SweepOnce(ctx context.Context) (int64, error) {
	if r.window <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-r.window)
	var total int64
	for {
		n, err := r.deleteBatch(ctx, cutoff)
		total += n
		if err != nil {
			return total, err
		}
		if n < int64(r.batch) {
			return total, nil // last (partial) batch drained the backlog
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

func (r *Retention) deleteBatch(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Scoped, transaction-local flag — the ONLY thing the trigger accepts as a
	// sanctioned delete. SET LOCAL does not leak past COMMIT/ROLLBACK.
	if _, err := tx.Exec(ctx, `SET LOCAL lens.audit_retention = 'on'`); err != nil {
		return 0, fmt.Errorf("audit retention: set flag: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM `+retentionTable+
			` WHERE ctid IN (SELECT ctid FROM `+retentionTable+` WHERE created_at < $1 LIMIT $2)`,
		cutoff, r.batch)
	if err != nil {
		return 0, fmt.Errorf("audit retention: delete batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("audit retention: commit: %w", err)
	}
	return tag.RowsAffected(), nil
}

// StartSweeper runs SweepOnce on a fixed interval until ctx is cancelled. A
// non-positive window does NOT start the loop (default-off). BLOCKS until ctx is
// done — intended to run under the HA leader (so exactly one instance sweeps the
// highest-volume table). Mirrors the semantic-cache DeleteStale/StartSweeper pattern.
func (r *Retention) StartSweeper(ctx context.Context, interval time.Duration) {
	if r.window <= 0 {
		r.log.Info("audit retention sweeper disabled (LENS_AUDIT_RETENTION unset/<=0)")
		return
	}
	r.log.Info("audit retention sweeper started", "window", r.window.String(), "interval", interval.String(), "table", retentionTable)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := r.SweepOnce(ctx)
			switch {
			case err != nil:
				r.log.Warn("audit retention sweep failed", "err", err)
			case n > 0:
				r.log.Info("audit retention sweep", "deleted", n, "table", retentionTable)
			}
		}
	}
}
