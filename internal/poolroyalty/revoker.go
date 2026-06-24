// revoker.go — the pool-mint REVOKE orchestrator (Stage 2.3b follow-up): turns
// the already-tested held-burn primitive (mining.RevokeHeldTx) into a real,
// auditable production operation. Given an EXPLICIT set of held request_ids, it
// revokes exactly those — NO automatic trigger, no detector wire (Stage 3 adds
// the gated trigger). Inert until then: nothing in production calls this yet.
//
// PER-ROW DISCIPLINE — one tx per request_id, CAS-FIRST:
//
//	tx := Begin
//	  UPDATE pool_royalty_mints SET status='revoked'
//	    WHERE request_id=$1 AND status='held'
//	    RETURNING contributor_workspace_id, minted_amount
//	  if a row came back  → RevokeHeldTx(contributor, amount) in the SAME tx → Commit  (revoked)
//	  else (0 rows)       → classify the skip (read-only) → Rollback                   (skipped_*)
//
// The status flip is the GATE: the held-burn fires ONLY on a row the CAS
// successfully transitioned, in the same tx, so a concurrent finalize or a
// double-call can never cause a double-burn or a burn without a flip. A
// finalized mint (status='final') matches 0 rows → never revocable. Each
// request_id is its own tx (per-row lock scope, matching the mint/finalize
// discipline) — never one tx across the whole set.
//
// MONEY CORRECTNESS lives ENTIRELY in the CAS + RETURNING + same-tx burn. The
// classify SELECT on the 0-row path is BEST-EFFORT/ADVISORY ONLY — it runs
// after the (write-free) tx has already decided to write nothing, purely to
// label the skip in the report. A row concurrently transitioned between the
// CAS and the classify SELECT can at worst MISLABEL a skip (e.g. report
// already_revoked vs not_held); it can NEVER cause a write or a burn, because
// nothing downstream of the classify writes anything.
//
// LOCKING: per revoke tx, two row locks in a CONSISTENT global order — the
// claim row (pool_royalty_mints, via the conditional UPDATE) THEN the
// contributor balance row (lens_token_balances, via RevokeHeldTx's
// SELECT ... FOR UPDATE). The finalize sweeper takes the SAME order (claim CAS
// then balance), so no cycle is constructible — no deadlock, no #32 surface
// (#32 governs two BALANCE rows; this locks one claim + one balance row).
package poolroyalty

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RevokeOutcome is the per-request_id result of a revoke attempt.
type RevokeOutcome string

const (
	OutcomeRevoked               RevokeOutcome = "revoked"
	OutcomeSkippedNotHeld        RevokeOutcome = "skipped_not_held"        // row is final (or any non-held, non-revoked) — never revocable
	OutcomeSkippedAlreadyRevoked RevokeOutcome = "skipped_already_revoked" // row already revoked — idempotent no-op
	OutcomeSkippedNotFound       RevokeOutcome = "skipped_not_found"       // no such request_id
	OutcomeError                 RevokeOutcome = "error"                   // DB / burn failure for this row (whole-row tx rolled back)
)

// RevokeReport is the auditable account of a RevokeHeldMints call: the
// per-request_id outcome and per-outcome totals.
type RevokeReport struct {
	Outcomes map[string]RevokeOutcome
	Totals   map[RevokeOutcome]int
}

// revokerDB is the minimal tx-begin seam (a nil pool degrades to a no-op).
type revokerDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// heldRevoker is the held-burn surface; *mining.LedgerStore.RevokeHeldTx
// satisfies it exactly (we CALL it, never change it).
type heldRevoker interface {
	RevokeHeldTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount float64, description string, metadata map[string]interface{}) error
}

// Revoker turns the held-burn primitive into a production operation. The
// zero/nil Revoker is inert. `table` scopes the CAS to one mint table
// (pool_royalty_mints or distill_royalty_mints).
type Revoker struct {
	db     revokerDB
	ledger heldRevoker
	table  string
}

// NewRevoker builds a revoke orchestrator over the cache royalty
// (pool_royalty_mints). For the distill table use NewRevokerForTable.
func NewRevoker(db revokerDB, ledger heldRevoker) *Revoker {
	return NewRevokerForTable(db, ledger, "pool_royalty_mints")
}

// NewRevokerForTable builds a revoke orchestrator over an EXPLICIT mint table.
// table is a hardcoded literal from the caller (never user input).
func NewRevokerForTable(db revokerDB, ledger heldRevoker, table string) *Revoker {
	return &Revoker{db: db, ledger: ledger, table: table}
}

// revokeCASSQLFor / classifyStatusSQLFor build the table-scoped revoke SQL. Both
// mint tables expose the same columns (request_id, status, contributor_workspace_id,
// minted_amount), so the identical CAS works for both. `table` is a hardcoded
// literal (NewRevoker / NewRevokerForTable), NEVER user input, so the fmt.Sprintf
// interpolation is injection-safe (mirrors sweeper.go's sweepSelectSQLFor).
//
// revokeCASSQLFor is CAS-first: flip held→revoked and RETURN the data the burn
// needs, in one statement. A returned row proves the flip happened; 0 rows means
// the row was not held (final/revoked/absent) — never burn.
func revokeCASSQLFor(table string) string {
	return fmt.Sprintf(`UPDATE %s SET status = 'revoked'
WHERE request_id = $1 AND status = 'held'
RETURNING contributor_workspace_id, minted_amount`, table)
}

// classifyStatusSQLFor is the ADVISORY skip-labeller (read-only; no money rides on it).
func classifyStatusSQLFor(table string) string {
	return fmt.Sprintf(`SELECT status FROM %s WHERE request_id = $1`, table)
}

// RevokeHeldMints revokes exactly the given held request_ids, each in its own
// tx, and returns an auditable per-row report. Idempotent: a second call on the
// same set revokes nothing (all skipped_already_revoked). A per-row DB error is
// recorded as OutcomeError and never fails the whole call.
func (r *Revoker) RevokeHeldMints(ctx context.Context, requestIDs []string) (RevokeReport, error) {
	rep := RevokeReport{Outcomes: map[string]RevokeOutcome{}, Totals: map[RevokeOutcome]int{}}
	if r == nil || r.db == nil || r.ledger == nil {
		return rep, nil
	}
	for _, id := range requestIDs {
		// Process each DISTINCT request_id once: a dup is the same logical mint,
		// and the per-row CAS already makes the burn idempotent — but skipping
		// it here keeps the audit report internally consistent
		// (sum(Totals) == len(Outcomes)) and avoids a redundant second tx.
		if _, seen := rep.Outcomes[id]; seen {
			continue
		}
		out := r.revokeOne(ctx, id)
		rep.Outcomes[id] = out
		rep.Totals[out]++
	}
	return rep, nil
}

// revokeOne handles a single request_id in its own transaction.
func (r *Revoker) revokeOne(ctx context.Context, requestID string) RevokeOutcome {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return OutcomeError
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var contributor string
	var amount float64
	err = tx.QueryRow(ctx, revokeCASSQLFor(r.table), requestID).Scan(&contributor, &amount)
	if err == nil {
		// The CAS transitioned the row (held → revoked) and returned its
		// contributor + amount. Burn from held in the SAME tx; commit only if
		// both succeed (atomic flip+burn).
		if berr := r.ledger.RevokeHeldTx(ctx, tx, contributor, amount,
			"pool royalty revoked (held mint clawed back)", map[string]interface{}{"request_id": requestID}); berr != nil {
			return OutcomeError // deferred rollback discards the flip with the failed burn
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			return OutcomeError
		}
		return OutcomeRevoked
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return OutcomeError
	}

	// 0 rows from the CAS: the row was not held. The money decision is already
	// made (nothing written; the deferred rollback ends the tx). The classify
	// SELECT below is ADVISORY ONLY — it just labels the skip for the report and
	// can never cause a write or a burn.
	var status string
	if serr := tx.QueryRow(ctx, classifyStatusSQLFor(r.table), requestID).Scan(&status); serr != nil {
		if errors.Is(serr, pgx.ErrNoRows) {
			return OutcomeSkippedNotFound
		}
		return OutcomeError
	}
	switch status {
	case "revoked":
		return OutcomeSkippedAlreadyRevoked
	default:
		// final (or any other non-held status) — not revocable.
		return OutcomeSkippedNotHeld
	}
}
