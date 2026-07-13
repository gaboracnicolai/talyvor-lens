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
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/dbjson"
)

// Stage-2.3a held-balance ledger row types. TypePoolRoyaltyHeld marks the
// mint-time held credit; TypePoolRoyaltyRevoked marks a confirmed-bad mint's
// burn-from-held. Both are deliberately ABSENT from GetTotalSupply's
// allow-list and from the burned list — see the package comment.
const (
	TypePoolRoyaltyHeld    = "pool_royalty_held"
	TypePoolRoyaltyRevoked = "pool_royalty_revoked"
	// TypeEvalContributionHeld is the proof-of-eval-contribution held mint moment (Proof-of-Improvement
	// instance 1): a contributor's held credit for a discriminating eval item. It is a HELD mint like
	// pool_royalty_held — gated by the U6 floor + 24h rate cap via mintTypeList — and settled by the
	// generic FinalizeSweeper over eval_contribution_mints. It is NOT reputation-bonded (absent from
	// isReputationBondedType): the bond code path runs and no-ops for this type — no logic change.
	TypeEvalContributionHeld = "eval_contribution_held"
	// TypeRoutingPredictionHeld is the proof-of-routing-prediction held mint moment (Proof-of-Improvement
	// instance 2): a contributor's held credit for a routing prediction proven skill-above-baseline on the
	// verifier-held eval slice. Like eval_contribution_held it is a HELD mint gated by the U6 floor + 24h
	// rate cap via mintTypeList and settled by the generic FinalizeSweeper (over routing_prediction_mints).
	// It is NOT reputation-bonded (absent from isReputationBondedType) — symmetric with the eval-contribution
	// mint: the bond code path runs and no-ops for this type.
	TypeRoutingPredictionHeld = "eval_routing_prediction_held"
	// TypeLatencyLocalityHeld is the proof-of-latency-locality held mint moment (Proof-of-Improvement
	// instance 3): a node's held credit for genuinely-fast, cohort-relative, quality-gated service. Like the
	// other two P-o-I mints it is a HELD mint gated by the U6 floor + 24h rate cap via mintTypeList and
	// settled by the generic FinalizeSweeper (over node_latency_mints). It is NOT reputation-bonded (absent
	// from isReputationBondedType): the bond code path runs and no-ops for this type — no logic change.
	TypeLatencyLocalityHeld = "eval_latency_locality_held"
	// TypeConfidentialComputeHeld is the proof-of-confidential-compute held mint moment (Proof-of-Improvement
	// instance 4): a node's held credit for providing VERIFIED confidential capacity (NVIDIA CC attestation +
	// key_bound + held-probe). Like the other three P-o-I mints it is a HELD mint gated by the U6 floor + 24h
	// rate cap via mintTypeList and settled by the generic FinalizeSweeper (over confidential_compute_mints).
	// NOT reputation-bonded (absent from isReputationBondedType): the bond code path runs and no-ops.
	TypeConfidentialComputeHeld = "eval_confidential_compute_held"
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
	transition func(bal, held, earned int64) (newBal, newHeld, newEarned, delta, balanceAfter int64, err error),
) error {
	// U6 Sybil floor: gate the held MINT (pool_royalty_held) — the worst Sybil
	// hole — on verified-to-earn. Finalize (pool_royalty) and revoke
	// (pool_royalty_revoked) are NOT mint types, so verifyEarn is a no-op for
	// them: finalize settles already-gated held value, revoke burns it. A block
	// rolls the whole tx back.
	if err := s.verifyEarn(ctx, tx, workspaceID, txType); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, heldEnsureBalanceSQL, workspaceID); err != nil {
		return fmt.Errorf("mining: ensure balance row: %w", err)
	}
	var bal, held, earned int64
	if err := tx.QueryRow(ctx, heldLockSelectSQL, workspaceID).Scan(&bal, &held, &earned); err != nil {
		return fmt.Errorf("mining: read held balance: %w", err)
	}

	newBal, newHeld, newEarned, delta, balanceAfter, err := transition(bal, held, earned)
	if err != nil {
		return err
	}

	// U6 PR2: rate cap on the held MINT (pool_royalty_held). checkMintRateCap is
	// a no-op for finalize (pool_royalty) and revoke (pool_royalty_revoked) — not
	// mint types. AFTER the FOR UPDATE (heldLockSelectSQL) so same-workspace mints
	// serialize. delta is the held credit amount (+) for the mint.
	if err := s.checkMintRateCap(ctx, tx, workspaceID, txType, delta); err != nil {
		return err
	}

	meta, err := dbjson.Marshal(metadata) // JSON text on both protocols (#133)
	if err != nil {
		return fmt.Errorf("mining: marshal metadata: %w", err)
	}

	if _, err := tx.Exec(ctx, heldLedgerInsertSQL,
		workspaceID, delta, balanceAfter, txType, description, meta); err != nil {
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
func (s *LedgerStore) CreditHeldTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, txType, description string, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: held credit amount must be positive")
	}
	// P1 #9: reputation bond on the held MINT (pool_royalty_held). Scale by f(R) / gate below floor
	// BEFORE the transition closure captures the amount. heldInner still runs verifyEarn (U6 floor)
	// + checkMintRateCap on the effective delta — compose, never bypass. No-op when off → byte-identical.
	eff, rerr := s.reputationBondedAmount(ctx, tx, workspaceID, txType, amount, metadata)
	if rerr != nil {
		return rerr
	}
	amount = eff
	return s.heldInner(ctx, tx, workspaceID, txType, description, metadata,
		func(bal, held, earned int64) (int64, int64, int64, int64, int64, error) {
			return bal, held + amount, earned, amount, bal, nil
		})
}

// FinalizeHeldTx moves `amount` LENS from held to spendable within a
// caller-supplied transaction — the settlement write. Conserves total owned
// value (bal+held), recognizes lifetime_earned, and writes the EXISTING
// counted TypePoolRoyalty ledger row — this is the moment the mint enters
// supply.
func (s *LedgerStore) FinalizeHeldTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, description string, metadata map[string]interface{}) error {
	return s.FinalizeHeldTxAs(ctx, tx, workspaceID, amount, TypePoolRoyalty, description, metadata)
}

// FinalizeHeldTxAs is FinalizeHeldTx with an explicit COUNTED ledger type (Phase-1
// Item 1). The pool-royalty finalize writes TypePoolRoyalty; the traffic-mint
// finalize writes the mint's own counted type (cache_mine / compute_mine /
// embedding_mine) so GetTotalSupply and the per-mint stats attribute it correctly.
// finalType MUST be a counted, non-mint-moment type (finalize settles already-gated
// held value — it is not a new mint, so it is NOT in mintTypeList and skips the
// verified-earn gate + rate cap).
func (s *LedgerStore) FinalizeHeldTxAs(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, finalType, description string, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: finalize amount must be positive")
	}
	return s.heldInner(ctx, tx, workspaceID, finalType, description, metadata,
		func(bal, held, earned int64) (int64, int64, int64, int64, int64, error) {
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
func (s *LedgerStore) RevokeHeldTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, description string, metadata map[string]interface{}) error {
	return s.RevokeHeldTxAs(ctx, tx, workspaceID, amount, TypePoolRoyaltyRevoked, description, metadata)
}

// RevokeHeldTxAs is RevokeHeldTx with an explicit revoke ledger type (Phase-1 Item
// 1). revokeType MUST be a non-mint, supply-neutral type (the held LENS never
// entered supply, so a revoke must not appear in the burned list) — GetTotalSupply
// and GetTotalBurned both exclude it. Reuse TypePoolRoyaltyRevoked for the traffic
// mints too: it is already excluded everywhere and carries the right semantics.
func (s *LedgerStore) RevokeHeldTxAs(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, revokeType, description string, metadata map[string]interface{}) error {
	if amount <= 0 {
		return errors.New("mining: revoke amount must be positive")
	}
	return s.heldInner(ctx, tx, workspaceID, revokeType, description, metadata,
		func(bal, held, earned int64) (int64, int64, int64, int64, int64, error) {
			if held < amount {
				return 0, 0, 0, 0, 0, ErrInsufficientHeld
			}
			return bal, held - amount, earned, -amount, bal, nil
		})
}
