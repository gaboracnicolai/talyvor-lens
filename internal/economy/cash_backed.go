package economy

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CASH-BACKED LXC — which part of a fungible balance real money paid for.
//
// ⚠ THE DEFECT THIS CLOSES. lxc_ledger rows are typed (purchase / admin_grant / convert_from_lens)
// but lxc_balances.balance is one BIGINT, and the settle path reads only that. Measured against
// real Postgres through the production seam, all three funding sources minted BYTE-IDENTICALLY —
// 376 µLENS each. The funding source was not under-weighted; it was invisible. A royalty minted
// against comped or converted credit is LENS issued with no cash behind it.
//
// ⚠ SPEND CONSUMES UNBACKED FIRST. Ordering does not change the TOTAL cash-backed spend over a
// fully-drained balance — it changes every PREFIX. A workspace holding 30 cash + 70 grant that
// spends 50 mints on 0 under unbacked-first and on 30 under cash-first. Unbacked-first therefore
// mints strictly less at every point in time, and it leaves cash_backed available for a later
// refund to decrement rather than clamping at zero with the backing already spent.
//
// This is lot accounting collapsed to one scalar: no lot table, no straddling rows, no new scan. It
// extends to fractional φ later by changing the unit to cash-backed µUSD.

// readCashBacked reads the cash-backed portion. The caller must already hold the balance row FOR
// UPDATE (readLXCBalance does), so this is a second read inside the same lock.
func readCashBacked(ctx context.Context, tx pgx.Tx, workspaceID string) (int64, error) {
	var v int64
	if err := tx.QueryRow(ctx,
		`SELECT cash_backed_ulxc FROM lxc_balances WHERE workspace_id = $1`, workspaceID).Scan(&v); err != nil {
		return 0, fmt.Errorf("economy: read cash-backed: %w", err)
	}
	return v, nil
}

func writeCashBacked(ctx context.Context, tx pgx.Tx, workspaceID string, v int64) error {
	if v < 0 {
		v = 0
	}
	if _, err := tx.Exec(ctx,
		`UPDATE lxc_balances SET cash_backed_ulxc = $2, updated_at = NOW() WHERE workspace_id = $1`,
		workspaceID, v); err != nil {
		return fmt.Errorf("economy: write cash-backed: %w", err)
	}
	return nil
}

// consumeCashBacked applies a SETTLED spend of `spend` µLXC against a balance of `balanceBefore`
// µLXC and returns how much of that spend was CASH-BACKED. It writes the new cash-backed figure.
//
// ⚠ balanceBefore MUST BE THE BALANCE WITH ANY HOLD ALREADY UNDONE. The reservation hold debits
// `balance` without touching cash_backed, so mid-hold `balance - cashBacked` is NEGATIVE and a
// naive subtraction produces nonsense. The settle path passes its post-release figure.
//
// The max(0, …) below is not decoration: a concurrent hold on the same workspace can still make
// unbacked read negative, and clamping there attributes MORE of the spend to cash — consuming
// backing faster and minting LESS in future. That is the safe direction to be wrong in.
func consumeCashBacked(ctx context.Context, tx pgx.Tx, workspaceID string, balanceBefore, spend int64) (fromCash int64, err error) {
	if spend <= 0 {
		return 0, nil
	}
	cashBacked, err := readCashBacked(ctx, tx, workspaceID)
	if err != nil {
		return 0, err
	}
	unbacked := balanceBefore - cashBacked
	if unbacked < 0 {
		unbacked = 0
	}
	fromUnbacked := spend
	if fromUnbacked > unbacked {
		fromUnbacked = unbacked
	}
	fromCash = spend - fromUnbacked
	if fromCash > cashBacked {
		fromCash = cashBacked // never consume more backing than exists
	}
	if fromCash > 0 {
		if err := writeCashBacked(ctx, tx, workspaceID, cashBacked-fromCash); err != nil {
			return 0, err
		}
	}
	return fromCash, nil
}

// addCashBacked increases the cash-backed portion. Called ONLY on a real purchase.
func addCashBacked(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64) error {
	if amount <= 0 {
		return nil
	}
	cur, err := readCashBacked(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	return writeCashBacked(ctx, tx, workspaceID, cur+amount)
}

// ReduceCashBackedForRefund removes backing that a refund took back out of the system.
//
// ⚠ WITHOUT THIS, A PURCHASE-THEN-REFUND LEAVES PHANTOM BACKING THAT MINTS FOREVER. Lens does not
// claw back the credit on a refund (#380 records the refund; it does not debit the balance), so the
// workspace keeps spendable LXC — but the cash behind it is gone, and every future settle against
// that portion would mint a royalty funded by money that was returned.
//
// Clamped at zero: a refund larger than the remaining backing means the backing was already spent,
// and there is nothing left to remove. Exported because the writer lives in internal/billing.
func ReduceCashBackedForRefund(ctx context.Context, tx pgx.Tx, workspaceID string, refundULXC int64) error {
	if refundULXC <= 0 {
		return nil
	}
	cur, err := readCashBacked(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	next := cur - refundULXC
	if next < 0 {
		next = 0
	}
	return writeCashBacked(ctx, tx, workspaceID, next)
}
