package mining

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// TypeTrafficMintRevoked is the burn ledger type RevokeHeldTxAs writes when a
// held traffic mint is clawed back. NOT a mint type (never in mintTypeList) — a
// burn, like pool_royalty_revoked.
const TypeTrafficMintRevoked = "traffic_mint_revoked"

// traffic_revoker.go — the clawback orchestrator for traffic_mint_holds (the
// cache / compute / embedding node mints). The poolroyalty.Revoker cannot reach
// this table: it RETURNs contributor_workspace_id and keys on request_id alone,
// but traffic_mint_holds uses workspace_id and a COMPOSITE key
// (request_id, workspace_id, mint_type). TrafficRevoker mirrors the Revoker's
// proven discipline — per-row tx, CAS-first (status held→revoked, RETURNING the
// burn inputs), same-tx RevokeHeldTxAs — over the composite key.

// TrafficHoldKey identifies one held traffic mint (the composite PK).
type TrafficHoldKey struct {
	RequestID   string
	WorkspaceID string
	MintType    string
}

func (k TrafficHoldKey) id() string { return k.RequestID + "|" + k.WorkspaceID + "|" + k.MintType }

// TrafficRevokeOutcome mirrors poolroyalty.RevokeOutcome (mining is the lower
// package, so it owns its own vocab — the meanings are identical).
type TrafficRevokeOutcome string

const (
	TrafficRevokeRevoked               TrafficRevokeOutcome = "revoked"
	TrafficRevokeSkippedNotHeld        TrafficRevokeOutcome = "skipped_not_held"
	TrafficRevokeSkippedAlreadyRevoked TrafficRevokeOutcome = "skipped_already_revoked"
	TrafficRevokeSkippedNotFound       TrafficRevokeOutcome = "skipped_not_found"
	TrafficRevokeError                 TrafficRevokeOutcome = "error"
)

// TrafficRevokeReport is the auditable per-key account of a RevokeTrafficHolds call.
type TrafficRevokeReport struct {
	Outcomes map[string]TrafficRevokeOutcome
	Totals   map[TrafficRevokeOutcome]int
}

// TrafficRevoker turns RevokeHeldTxAs into a production clawback over
// traffic_mint_holds. A nil pool/ledger is inert.
type TrafficRevoker struct {
	pool   pgxDB
	ledger *LedgerStore
}

func NewTrafficRevoker(pool pgxDB, ledger *LedgerStore) *TrafficRevoker {
	return &TrafficRevoker{pool: pool, ledger: ledger}
}

// trafficRevokeCASSQL flips one held row → revoked and RETURNs the burn input.
// A returned row proves the flip; 0 rows means not-held (final/revoked/absent) —
// never burn. Keyed on the FULL composite PK (request_id, workspace_id, mint_type),
// so it targets exactly one row.
const trafficRevokeCASSQL = `UPDATE traffic_mint_holds SET status = 'revoked'
WHERE request_id = $1 AND workspace_id = $2 AND mint_type = $3 AND status = 'held'
RETURNING minted_amount`

// trafficClassifySQL is the ADVISORY skip-labeller (read-only; no money rides on it).
const trafficClassifySQL = `SELECT status FROM traffic_mint_holds
WHERE request_id = $1 AND workspace_id = $2 AND mint_type = $3`

// RevokeTrafficHolds revokes exactly the given held keys, each in its own tx, and
// returns an auditable per-key report. Idempotent: a second call revokes nothing
// (skipped_already_revoked). Mirrors poolroyalty.Revoker's per-row CAS-first
// discipline (the status flip is the gate; the held-burn fires ONLY on a row the
// CAS transitioned, in the SAME tx — so a concurrent finalize or a double-call can
// never double-burn or burn without a flip). A finalized row matches 0 rows → never
// revocable.
func (r *TrafficRevoker) RevokeTrafficHolds(ctx context.Context, keys []TrafficHoldKey) (TrafficRevokeReport, error) {
	rep := TrafficRevokeReport{Outcomes: map[string]TrafficRevokeOutcome{}, Totals: map[TrafficRevokeOutcome]int{}}
	if r == nil || r.pool == nil || r.ledger == nil {
		return rep, nil
	}
	for _, k := range keys {
		if _, seen := rep.Outcomes[k.id()]; seen {
			continue // process each distinct key once (the CAS already makes the burn idempotent)
		}
		out := r.revokeOne(ctx, k)
		rep.Outcomes[k.id()] = out
		rep.Totals[out]++
	}
	return rep, nil
}

// revokeOne handles a single composite key in its own transaction.
func (r *TrafficRevoker) revokeOne(ctx context.Context, k TrafficHoldKey) TrafficRevokeOutcome {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return TrafficRevokeError
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var amount int64 // µLENS (traffic_mint_holds.minted_amount is BIGINT)
	err = tx.QueryRow(ctx, trafficRevokeCASSQL, k.RequestID, k.WorkspaceID, k.MintType).Scan(&amount)
	if err == nil {
		// The CAS transitioned held→revoked and returned the amount. Burn from held
		// in the SAME tx; commit only if both succeed (atomic flip+burn).
		if berr := r.ledger.RevokeHeldTxAs(ctx, tx, k.WorkspaceID, amount, TypeTrafficMintRevoked,
			"traffic mint revoked (held clawed back)", map[string]interface{}{"request_id": k.RequestID, "mint_type": k.MintType}); berr != nil {
			return TrafficRevokeError // deferred rollback discards the flip with the failed burn
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			return TrafficRevokeError
		}
		return TrafficRevokeRevoked
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TrafficRevokeError
	}

	// 0 rows from the CAS: the row was not held. The money decision is already made
	// (nothing written). The classify SELECT is ADVISORY ONLY — it labels the skip.
	var status string
	if serr := tx.QueryRow(ctx, trafficClassifySQL, k.RequestID, k.WorkspaceID, k.MintType).Scan(&status); serr != nil {
		if errors.Is(serr, pgx.ErrNoRows) {
			return TrafficRevokeSkippedNotFound
		}
		return TrafficRevokeError
	}
	switch status {
	case "revoked":
		return TrafficRevokeSkippedAlreadyRevoked
	default:
		return TrafficRevokeSkippedNotHeld // final (or any non-held) — not revocable
	}
}
