// sweeper.go — the Stage-2.3a finalize sweeper: settles due held mints
// (held → spendable) after the holdback window.
//
// Mirrors the povi-challenge scheduler exactly: registered in main under
// haComps.leader.Run("pool-royalty-finalize", ...) — leader-elected via
// Redis SetNX when HA is on, direct-run when off — ticking on a minute
// scale. Registered UNCONDITIONALLY (never gated on the minting flag):
// committed held rows must finalize on schedule even if minting is later
// disabled, or contributor LENS strands in held forever.
//
// Each due row settles in its OWN single transaction, claim-first:
//
//	(1) CAS: UPDATE pool_royalty_mints SET status='final'
//	         WHERE request_id=$1 AND status='held'
//	    RowsAffected()==0 → another sweeper already settled it (HA failover
//	    overlap) → roll back and skip BEFORE touching any balance. Double-
//	    finalize is impossible by this guard — the same claim/RowsAffected
//	    discipline as povi_challenges, the 2.1 mint claim, and the 2.2
//	    RecordReceipt fix.
//	(2) FinalizeHeldTx: the single-row FOR UPDATE held→spendable move, which
//	    writes the EXISTING counted TypePoolRoyalty ledger row — the moment
//	    the mint enters supply.
//
// Two single-row writes in one tx, no cross-row ordering surface (each claim
// row is only ever touched by its own request_id-keyed transition; distinct
// claim rows mean no lock cycle is constructible). Plain parameterized SQL —
// PgBouncer transaction-pooling safe.
//
// The settlement TRIGGER is decoupled from the ledger ops by design: this
// timed sweeper is the initial trigger; billing settlement can replace it
// later by calling the same per-row settle without schema or kernel changes.
package poolroyalty

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/metrics"
)

// sweepBatchLimit bounds one tick's settle work. NOT a silent cap: rows past
// the limit are simply settled on the next minute tick, and RunOnce logs
// when a full batch suggests more are waiting.
const sweepBatchLimit = 500

// sweepSelectSQL finds due held mints. Uses the partial index
// idx_pool_royalty_mints_finalize (finalize_after) WHERE status='held'.
// LIMIT derived from sweepBatchLimit so the constant can't drift.
var sweepSelectSQL = fmt.Sprintf(`SELECT request_id, contributor_workspace_id, minted_amount
FROM pool_royalty_mints
WHERE status = 'held' AND finalize_after < now()
LIMIT %d`, sweepBatchLimit)

// finalizeCASSQL claims the row for settlement — the double-finalize guard.
const finalizeCASSQL = `UPDATE pool_royalty_mints SET status = 'final'
WHERE request_id = $1 AND status = 'held'`

type sweeperDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// heldFinalizer is the minimal settle surface; *mining.LedgerStore's
// FinalizeHeldTx satisfies it exactly.
type heldFinalizer interface {
	FinalizeHeldTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount float64, description string, metadata map[string]interface{}) error
}

// FinalizeSweeper settles due held mints (Stage 2.3a). The zero/nil sweeper
// is inert.
type FinalizeSweeper struct {
	db     sweeperDB
	ledger heldFinalizer
}

// NewFinalizeSweeper wires the pool and the held ledger.
func NewFinalizeSweeper(db sweeperDB, ledger heldFinalizer) *FinalizeSweeper {
	return &FinalizeSweeper{db: db, ledger: ledger}
}

type dueMint struct {
	requestID   string
	contributor string
	amount      float64
}

// RunOnce sweeps due held rows and settles each in its own CAS-guarded tx.
// Returns the number finalized. Per-row failures are logged and skipped (the
// row stays 'held' and retries next tick); only the sweep SELECT itself can
// error the call.
func (s *FinalizeSweeper) RunOnce(ctx context.Context) (int, error) {
	if s == nil || s.db == nil || s.ledger == nil {
		return 0, nil
	}
	rows, err := s.db.Query(ctx, sweepSelectSQL)
	if err != nil {
		return 0, err
	}
	due := make([]dueMint, 0, 16)
	for rows.Next() {
		var d dueMint
		if err := rows.Scan(&d.requestID, &d.contributor, &d.amount); err != nil {
			rows.Close()
			return 0, err
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	finalized := 0
	for _, d := range due {
		if err := s.settleOne(ctx, d); err != nil {
			if _, lost := err.(casLost); lost {
				continue // normal HA overlap — the other sweeper settled it
			}
			slog.Warn("poolroyalty: finalize failed (row stays held; retries next tick)",
				slog.String("request_id", d.requestID),
				slog.String("contributor", d.contributor),
				slog.String("error", err.Error()),
			)
			continue
		}
		finalized++
	}
	if len(due) == sweepBatchLimit {
		slog.Info("poolroyalty: finalize sweep hit batch limit — more due rows settle next tick",
			slog.Int("batch", sweepBatchLimit))
	}
	return finalized, nil
}

// errCASLost is internal: another sweeper settled the row first.
type casLost struct{}

func (casLost) Error() string { return "finalize CAS lost (already settled)" }

// settleOne settles a single due mint: CAS-claim the row, then move
// held→spendable, in one transaction. A lost CAS is a silent skip (not an
// error and not a finalize).
func (s *FinalizeSweeper) settleOne(ctx context.Context, d dueMint) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, finalizeCASSQL, d.requestID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Another sweeper (HA overlap) already settled it — skip before
		// touching any balance. The deferred rollback ends the tx.
		return casLost{}
	}
	meta := map[string]interface{}{"request_id": d.requestID}
	if err := s.ledger.FinalizeHeldTx(ctx, tx, d.contributor, d.amount,
		"pool royalty finalized (holdback window elapsed)", meta); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// The mint enters circulation NOW (the counted TypePoolRoyalty row was
	// just committed) — this is where the supply counter agrees with SQL.
	metrics.MintedTokens(d.amount)
	slog.Info("poolroyalty: royalty finalized (held → spendable)",
		slog.String("request_id", d.requestID),
		slog.String("contributor", d.contributor),
		slog.Float64("amount", d.amount),
	)
	return nil
}

// StartScheduler ticks RunOnce until ctx ends — mirrors
// povi.ChallengeScheduler.StartScheduler.
func (s *FinalizeSweeper) StartScheduler(ctx context.Context, tick time.Duration) {
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
			if _, err := s.RunOnce(ctx); err != nil {
				slog.Warn("poolroyalty: finalize sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}
