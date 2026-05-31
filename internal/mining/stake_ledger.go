package mining

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/metrics"
)

// Stake ledger operations (PoVI Part 2). Staking is COLLATERAL, not payment:
// locked LENS is still owned by the operator, just held in a locked state. So
// it moves available↔locked↔burned as a real, audited balance transition —
// distinct from Credit/Debit (which model payment, where LENS genuinely leaves
// or enters). Each op is a single FOR-UPDATE transaction so concurrent
// stake/unbond/slash can't double-spend or lose/create LENS.
//
// These add the locked_balance column mechanics WITHOUT touching the existing
// Credit/Debit/Transfer/Burn paths (which ignore locked_balance).

const (
	// TypeStakeLock records LENS moved available→locked as collateral.
	TypeStakeLock = "povi_stake_lock"
	// TypeStakeRelease records LENS moved locked→available (collateral returned).
	TypeStakeRelease = "povi_stake_release"
	// TypeStakeSlash records LENS burned out of locked (collateral forfeited;
	// supply reduced).
	TypeStakeSlash = "povi_stake_slash"
)

// ErrInsufficientLocked is returned when a release/slash exceeds the locked
// balance.
var ErrInsufficientLocked = errors.New("mining: insufficient locked balance")

// Two-step explicit lock: ensure the row exists (DO NOTHING so no spurious
// updated_at touch), then acquire a row-level write lock with FOR UPDATE.
const stakeEnsureBalanceSQL = `
	INSERT INTO lens_token_balances (workspace_id, balance, lifetime_earned, lifetime_spent)
	VALUES ($1, 0, 0, 0) ON CONFLICT (workspace_id) DO NOTHING`

const stakeLockSelectSQL = `
	SELECT balance, locked_balance
	FROM lens_token_balances WHERE workspace_id = $1 FOR UPDATE`

const stakeLedgerInsertSQL = `
	INSERT INTO lens_token_ledger (workspace_id, amount, balance_after, type, description, metadata)
	VALUES ($1, $2, $3, $4, $5, $6)`

const stakeBalanceUpdateSQL = `
	UPDATE lens_token_balances
	SET balance = $2, locked_balance = $3, updated_at = NOW()
	WHERE workspace_id = $1`

// LockStake atomically moves `amount` LENS from available balance to locked.
// Returns ErrInsufficientBalance when the workspace can't cover it.
func (s *LedgerStore) LockStake(ctx context.Context, workspaceID string, amount float64, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: stake lock amount must be positive")
	}
	return s.stakeTx(ctx, workspaceID, amount, TypeStakeLock, "stake locked (collateral)", metadata,
		func(bal, locked float64) (newBal, newLocked, delta, balanceAfter float64, err error) {
			if bal < amount {
				return 0, 0, 0, 0, ErrInsufficientBalance
			}
			newBal = bal - amount
			newLocked = locked + amount
			return newBal, newLocked, -amount, newBal, nil
		})
}

// ReleaseStake atomically moves `amount` LENS from locked back to available.
func (s *LedgerStore) ReleaseStake(ctx context.Context, workspaceID string, amount float64, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: stake release amount must be positive")
	}
	return s.stakeTx(ctx, workspaceID, amount, TypeStakeRelease, "stake released (collateral returned)", metadata,
		func(bal, locked float64) (newBal, newLocked, delta, balanceAfter float64, err error) {
			if locked < amount {
				return 0, 0, 0, 0, ErrInsufficientLocked
			}
			newBal = bal + amount
			newLocked = locked - amount
			return newBal, newLocked, amount, newBal, nil
		})
}

// SlashStake atomically BURNS `amount` LENS out of locked — it leaves locked and
// does NOT return to available, reducing total supply. Available balance is
// unchanged.
func (s *LedgerStore) SlashStake(ctx context.Context, workspaceID string, amount float64, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: stake slash amount must be positive")
	}
	return s.stakeTx(ctx, workspaceID, amount, TypeStakeSlash, "stake slashed (collateral burned)", metadata,
		func(bal, locked float64) (newBal, newLocked, delta, balanceAfter float64, err error) {
			if locked < amount {
				return 0, 0, 0, 0, ErrInsufficientLocked
			}
			// balance unchanged; the slashed amount is burned out of locked.
			return bal, locked - amount, -amount, bal, nil
		})
}

// LockedBalance returns the workspace's currently-locked LENS (0 if no row).
func (s *LedgerStore) LockedBalance(ctx context.Context, workspaceID string) (float64, error) {
	if s.pool == nil {
		return 0, nil
	}
	var locked float64
	err := s.pool.QueryRow(ctx,
		`SELECT locked_balance FROM lens_token_balances WHERE workspace_id = $1`, workspaceID,
	).Scan(&locked)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return locked, nil
}

// stakeInner executes the balance lock, ledger insert, and balance update for a
// stake operation within an already-begun transaction. It does NOT Begin or
// Commit — the caller controls the transaction boundary. This is the shared
// kernel used by both the self-contained stakeTx and the Tx-accepting variants
// that let StakeManager wrap multiple operations in one transaction.
func (s *LedgerStore) stakeInner(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	amount float64,
	txType, description string,
	metadata map[string]interface{},
	transition func(bal, locked float64) (newBal, newLocked, delta, balanceAfter float64, err error),
) error {
	if _, err := tx.Exec(ctx, stakeEnsureBalanceSQL, workspaceID); err != nil {
		return fmt.Errorf("mining: ensure balance row: %w", err)
	}
	var bal, locked float64
	if err := tx.QueryRow(ctx, stakeLockSelectSQL, workspaceID).Scan(&bal, &locked); err != nil {
		return fmt.Errorf("mining: read balance: %w", err)
	}

	newBal, newLocked, delta, balanceAfter, err := transition(bal, locked)
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

	if _, err := tx.Exec(ctx, stakeLedgerInsertSQL,
		workspaceID, delta, balanceAfter, txType, description, metaBuf); err != nil {
		return fmt.Errorf("mining: insert stake ledger row: %w", err)
	}
	if _, err := tx.Exec(ctx, stakeBalanceUpdateSQL, workspaceID, newBal, newLocked); err != nil {
		return fmt.Errorf("mining: update balance: %w", err)
	}
	return nil
}

// stakeTx is the self-contained path (Begin + stakeInner + Commit) used by the
// public LockStake / ReleaseStake / SlashStake methods that manage their own
// transaction.
func (s *LedgerStore) stakeTx(
	ctx context.Context,
	workspaceID string,
	amount float64,
	txType, description string,
	metadata map[string]interface{},
	transition func(bal, locked float64) (newBal, newLocked, delta, balanceAfter float64, err error),
) error {
	if s.pool == nil {
		return nil
	}
	start := time.Now()
	defer func() { metrics.ObserveLedgerWrite(time.Since(start)) }()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mining: begin stake tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.stakeInner(ctx, tx, workspaceID, amount, txType, description, metadata, transition); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LockStakeTx, ReleaseStakeTx, SlashStakeTx are the caller-tx variants: they
// perform the same balance transitions as their non-Tx counterparts but within
// an external transaction supplied by the caller (used by StakeManager to bundle
// the ledger op + stake-record update into one atomic transaction).

func (s *LedgerStore) LockStakeTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount float64, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: stake lock amount must be positive")
	}
	return s.stakeInner(ctx, tx, workspaceID, amount, TypeStakeLock, "stake locked (collateral)", metadata,
		func(bal, locked float64) (newBal, newLocked, delta, balanceAfter float64, err error) {
			if bal < amount {
				return 0, 0, 0, 0, ErrInsufficientBalance
			}
			return bal - amount, locked + amount, -amount, bal - amount, nil
		})
}

func (s *LedgerStore) ReleaseStakeTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount float64, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: stake release amount must be positive")
	}
	return s.stakeInner(ctx, tx, workspaceID, amount, TypeStakeRelease, "stake released (collateral returned)", metadata,
		func(bal, locked float64) (newBal, newLocked, delta, balanceAfter float64, err error) {
			if locked < amount {
				return 0, 0, 0, 0, ErrInsufficientLocked
			}
			return bal + amount, locked - amount, amount, bal + amount, nil
		})
}

func (s *LedgerStore) SlashStakeTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount float64, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: stake slash amount must be positive")
	}
	return s.stakeInner(ctx, tx, workspaceID, amount, TypeStakeSlash, "stake slashed (collateral burned)", metadata,
		func(bal, locked float64) (newBal, newLocked, delta, balanceAfter float64, err error) {
			if locked < amount {
				return 0, 0, 0, 0, ErrInsufficientLocked
			}
			return bal, locked - amount, -amount, bal, nil
		})
}
