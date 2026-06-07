// held_ledger.go — the Stage-2.3a HOLDBACK kernel for Pool-B royalty mints.
//
// A pool-royalty mint credits held_balance (unfinalized income) instead of
// spendable balance; the finalize sweeper settles held → spendable after the
// holdback window; a confirmed-bad mint is revoked by burning from held.
//
// This is a PARALLEL kernel deliberately copying the stakeInner PATTERN
// (stake_ledger.go) over the NEW held_balance column — it shares not one
// line or constant with the stake code, and never touches locked_balance
// (staking COLLATERAL, migration 0032) or any stake function. Same
// discipline: one workspace-keyed lens_token_balances row, the two-step
// ensure + SELECT ... FOR UPDATE, a pure transition closure, a ledger row,
// a balance update. No new lock species, no ordering question.
//
// SUPPLY SEMANTICS (Stage 2.3a): supply counts at FINALIZE, not mint —
// implemented by WHERE the counted type is written, not by changing any
// reader. CreditHeldTx writes TypePoolRoyaltyHeld and RevokeHeldTx writes
// TypePoolRoyaltyRevoked — neither is in GetTotalSupply's allow-list
// (uncounted by omission, like receipt_mine_provisional / marketplace_fee).
// FinalizeHeldTx writes TypePoolRoyalty — the existing counted type. The
// revoked type must also NEVER join the burned list (TypeBurn /
// TypeStakeSlash): held LENS never entered supply, so revoking it must not
// decrease circulating supply.
//
// lifetime_earned is recognized at FINALIZE (income isn't earned until
// final), so CreditHeldTx and RevokeHeldTx leave it untouched.
package mining

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Stage-2.3a held-balance ledger row types. TypePoolRoyaltyHeld marks the
// mint-time held credit; TypePoolRoyaltyRevoked marks a confirmed-bad mint's
// burn-from-held. Both are deliberately ABSENT from GetTotalSupply's
// allow-list and from the burned list — see the package comment.
const (
	TypePoolRoyaltyHeld    = "pool_royalty_held"
	TypePoolRoyaltyRevoked = "pool_royalty_revoked"
)

// ErrInsufficientHeld is returned when held_balance can't cover a finalize
// or revoke — the tx rolls back, nothing partial persists.
var ErrInsufficientHeld = errors.New("mining: insufficient held balance")

// Two-step explicit lock over the held column — own constants, zero coupling
// to the stake kernel's.
const heldEnsureBalanceSQL = `
	INSERT INTO lens_token_balances (workspace_id, balance, lifetime_earned, lifetime_spent)
	VALUES ($1, 0, 0, 0) ON CONFLICT (workspace_id) DO NOTHING`

const heldLockSelectSQL = `
	SELECT balance, held_balance, lifetime_earned
	FROM lens_token_balances WHERE workspace_id = $1 FOR UPDATE`

const heldLedgerInsertSQL = `
	INSERT INTO lens_token_ledger (workspace_id, amount, balance_after, type, description, metadata)
	VALUES ($1, $2, $3, $4, $5, $6)`

const heldBalanceUpdateSQL = `
	UPDATE lens_token_balances
	SET balance = $2, held_balance = $3, lifetime_earned = $4, updated_at = NOW()
	WHERE workspace_id = $1`

// heldInner executes the balance lock, ledger insert, and balance update for
// a held-balance operation within an already-begun transaction. It does NOT
// Begin or Commit — the caller owns the transaction boundary (the mint tx,
// or the sweeper's per-row settle tx).
func (s *LedgerStore) heldInner(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	txType, description string,
	metadata map[string]interface{},
	transition func(bal, held, earned float64) (newBal, newHeld, newEarned, delta, balanceAfter float64, err error),
) error {
	if _, err := tx.Exec(ctx, heldEnsureBalanceSQL, workspaceID); err != nil {
		return fmt.Errorf("mining: ensure balance row: %w", err)
	}
	var bal, held, earned float64
	if err := tx.QueryRow(ctx, heldLockSelectSQL, workspaceID).Scan(&bal, &held, &earned); err != nil {
		return fmt.Errorf("mining: read held balance: %w", err)
	}

	newBal, newHeld, newEarned, delta, balanceAfter, err := transition(bal, held, earned)
	if err != nil {
		return err
	}

	metaBuf, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("mining: marshal metadata: %w", err)
	}
	if string(metaBuf) == "null" {
		metaBuf = []byte("{}")
	}

	if _, err := tx.Exec(ctx, heldLedgerInsertSQL,
		workspaceID, delta, balanceAfter, txType, description, metaBuf); err != nil {
		return fmt.Errorf("mining: insert held ledger row: %w", err)
	}
	if _, err := tx.Exec(ctx, heldBalanceUpdateSQL, workspaceID, newBal, newHeld, newEarned); err != nil {
		return fmt.Errorf("mining: update held balance: %w", err)
	}
	return nil
}

// CreditHeldTx credits `amount` LENS to the workspace's HELD balance within a
// caller-supplied transaction — the Stage-2.3a mint write. Spendable balance
// and lifetime_earned are untouched (earnings recognize at finalize). The
// ledger row records +amount against the UNCHANGED spendable balance_after,
// with the caller's txType (TypePoolRoyaltyHeld from the mint path).
func (s *LedgerStore) CreditHeldTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount float64, txType, description string, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: held credit amount must be positive")
	}
	return s.heldInner(ctx, tx, workspaceID, txType, description, metadata,
		func(bal, held, earned float64) (float64, float64, float64, float64, float64, error) {
			return bal, held + amount, earned, amount, bal, nil
		})
}

// FinalizeHeldTx moves `amount` LENS from held to spendable within a
// caller-supplied transaction — the settlement write. Conserves total owned
// value (bal+held), recognizes lifetime_earned, and writes the EXISTING
// counted TypePoolRoyalty ledger row — this is the moment the mint enters
// supply.
func (s *LedgerStore) FinalizeHeldTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount float64, description string, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: finalize amount must be positive")
	}
	return s.heldInner(ctx, tx, workspaceID, TypePoolRoyalty, description, metadata,
		func(bal, held, earned float64) (float64, float64, float64, float64, float64, error) {
			if held < amount {
				return 0, 0, 0, 0, 0, ErrInsufficientHeld
			}
			newBal := bal + amount
			return newBal, held - amount, earned + amount, amount, newBal, nil
		})
}

// RevokeHeldTx burns `amount` LENS out of held within a caller-supplied
// transaction — the confirmed-bad-mint write. Spendable and lifetime_earned
// untouched; the held LENS never entered supply, so this must not (and does
// not) appear in the burned list either.
func (s *LedgerStore) RevokeHeldTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount float64, description string, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: revoke amount must be positive")
	}
	return s.heldInner(ctx, tx, workspaceID, TypePoolRoyaltyRevoked, description, metadata,
		func(bal, held, earned float64) (float64, float64, float64, float64, float64, error) {
			if held < amount {
				return 0, 0, 0, 0, 0, ErrInsufficientHeld
			}
			return bal, held - amount, earned, -amount, bal, nil
		})
}
