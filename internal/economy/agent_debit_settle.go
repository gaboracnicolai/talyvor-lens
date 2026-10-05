package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// agent_debit_settle.go — B23.13: WITH RESERVATIONS OFF, AN AGENT PAYS WHAT ITS QUESTION ACTUALLY COST.
//
// With LXCReservationEnabled off an agent's question is charged by SpendLXCForAgent's pre-serve debit, the
// input-only estimate. That debit stays the ceiling check it is; after the serve SettleAgentDebit settles the
// difference in ONE lxc_ledger row. Decided by Nicolai, 30 Sep 2026: never take an agent past its owner's
// spending limit; within the limit, charge what the answer actually cost; write off only the part the limit
// does not allow. The limit is read under the same locks the pre-serve debit takes (the key's sub-budget row,
// then the agent's), so two concurrent questions cannot together pass it.

// AgentDebitSettlement is what SettleAgentDebit did with one question's delivered cost.
type AgentDebitSettlement struct {
	// SettledULXC is the settling row: > 0 charged beyond the estimate, < 0 refunded, 0 nothing to settle.
	SettledULXC int64
	// WrittenOffULXC is the part of the delivered cost the agent's limit did not allow.
	WrittenOffULXC int64
	// CashBackedULXC is the part of the question's whole charge (the estimate plus SettledULXC) that may fund
	// a royalty — what SettleLXCReservation's cash-backed figure is on the reservation path (B27.6).
	CashBackedULXC int64
	// SettledFeeULXC is the platform fee's settling row (B32.11): the fee on the charge settled to, less the fee
	// the estimate was debited — > 0 charged, < 0 refunded.
	SettledFeeULXC int64
}

// SettleAgentDebit settles the pre-serve debit booked under debitKey to deliveredLXC, exactly once per
// debitKey: it refunds what the estimate over-charged, or charges what it under-charged capped at what the
// key's limit still allows, and records the rest as written off (agent_debit_settlements, with meta.RequestID,
// the question). A replay settles nothing and returns the zero settlement.
func (s *DualTokenStore) SettleAgentDebit(ctx context.Context, workspaceID, debitKey string, deliveredLXC int64, meta AgentDebitMeta) (AgentDebitSettlement, error) {
	var out AgentDebitSettlement
	if workspaceID == "" || debitKey == "" {
		return out, errors.New("economy: agent debit settle requires workspace_id, debit key")
	}
	if deliveredLXC < 0 {
		deliveredLXC = 0
	}
	if s == nil || s.pool == nil {
		return out, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("economy: begin agent debit settle: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The pre-serve debit's claim is the estimate that was charged — from the row, not from the caller.
	// So is the platform fee's rate (B32.11): the estimate was debited its fee at it.
	var scopedKeyID string
	var estimate, bps int64
	err = tx.QueryRow(ctx, `SELECT scoped_key_id, lxc_amount, platform_fee_bps FROM lxc_spend_claims WHERE request_id = $1`, debitKey).
		Scan(&scopedKeyID, &estimate, &bps)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("economy: settle of an agent debit never booked (%q)", debitKey)
	}
	if err != nil {
		return out, fmt.Errorf("economy: read agent debit claim: %w", err)
	}
	// Exactly once: the settlement's own claim. A second settle of this debit finds it and settles nothing.
	tag, err := tx.Exec(ctx, `INSERT INTO agent_debit_settlements
		(debit_key, workspace_id, scoped_key_id, request_id, estimate_ulxc, delivered_ulxc, settled_ulxc)
		VALUES ($1, $2, $3, $4, $5::bigint, $6::bigint, $6::bigint - $5::bigint) ON CONFLICT (debit_key) DO NOTHING`,
		debitKey, workspaceID, scopedKeyID, nullIfEmpty(meta.RequestID), estimate, deliveredLXC)
	if err != nil {
		return out, fmt.Errorf("economy: agent debit settlement claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return out, nil
	}

	// The locks the pre-serve debit takes, in its order: the key's sub-budget row, the agent, the balance.
	var ceiling, spent int64
	if err := tx.QueryRow(ctx,
		`SELECT ceiling_lxc, spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1 FOR UPDATE`,
		scopedKeyID).Scan(&ceiling, &spent); err != nil {
		return out, fmt.Errorf("economy: read sub-budget: %w", err)
	}
	var agentID string
	err = tx.QueryRow(ctx, `SELECT a.id FROM agent_account_keys k JOIN agent_accounts a ON a.id = k.agent_id
		WHERE k.scoped_key_id = $1 AND a.workspace_id = $2`, scopedKeyID, workspaceID).Scan(&agentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("economy: agent of key: %w", err)
	}
	if agentID != "" {
		if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
			return out, err
		}
	}

	// B32.11: the limit and the balance judge the whole price, the charge and its platform fee. The estimate was
	// debited estimate + estFee; the question costs delivered + its fee; settle the difference within what the
	// limit allows, then split the whole price that fits back into a charge and the fee on it.
	estFee := PlatformFee(estimate, bps)
	charge := deliveredLXC
	if diff := deliveredLXC + PlatformFee(deliveredLXC, bps) - estimate - estFee; diff > 0 {
		allowed := diff
		if agentID != "" {
			if allowed, err = agentAllowance(ctx, tx, workspaceID, agentID, estimate+estFee, diff, debitKey, meta.RequestedModel); err != nil {
				return out, err
			}
		} else {
			allowed = min(allowed, max(ceiling-spent, 0))
		}
		bal, _, _, err := readLXCBalance(ctx, tx, workspaceID)
		if err != nil {
			return out, err
		}
		if agentID == "" { // B19.13: a key attached to no agent spends only what its agents do not hold
			var allocated int64
			if err := tx.QueryRow(ctx, allocatedSQL, workspaceID).Scan(&allocated); err != nil {
				return out, fmt.Errorf("economy: allocated LXC: %w", err)
			}
			bal -= allocated
		}
		charge = min(deliveredLXC, spendWithin(estimate+estFee+min(allowed, max(bal, 0)), bps))
	}
	out.SettledULXC = charge - estimate // ≤ 0: the estimate over-charged, refund it
	out.WrittenOffULXC = deliveredLXC - charge
	out.SettledFeeULXC = PlatformFee(charge, bps) - estFee

	if out.SettledULXC != 0 || out.SettledFeeULXC != 0 {
		bal, minted, wsSpent, err := readLXCBalance(ctx, tx, workspaceID)
		if err != nil {
			return out, err
		}
		newBal := bal - out.SettledULXC
		if out.SettledULXC != 0 {
			desc := "agent debit settle: delivered cost above the estimate"
			if out.SettledULXC < 0 {
				desc = "agent debit settle: estimate above the delivered cost, refunded"
			}
			// A pooled cache serve's row says what the question would have cost and what it saved (B26.9).
			if err := insertLXCLedger(ctx, tx, workspaceID, -out.SettledULXC, newBal, LXCTypeSpend, desc,
				meta.toSpendMap(charge)); err != nil {
				return out, err
			}
		}
		if err := insertPlatformFee(ctx, tx, workspaceID, out.SettledFeeULXC, newBal-out.SettledFeeULXC, bps, charge, meta.RequestID); err != nil {
			return out, err
		}
		newBal -= out.SettledFeeULXC
		settled := out.SettledULXC + out.SettledFeeULXC
		if err := writeLXCBalance(ctx, tx, workspaceID, newBal, minted, wsSpent+settled); err != nil {
			return out, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE agent_lxc_subbudgets SET spent_lxc = spent_lxc + $2, updated_at = now() WHERE scoped_key_id = $1`,
			scopedKeyID, settled); err != nil {
			return out, fmt.Errorf("economy: bump spent (settle): %w", err)
		}
		if agentID != "" && out.SettledULXC != 0 {
			kind := "spend"
			if out.SettledULXC < 0 {
				kind = "settle"
			}
			if err := postModelEntry(ctx, tx, workspaceID, kind, debitKey, meta.RequestedModel,
				leg{agentAccount(agentID), -out.SettledULXC}, leg{"spend", out.SettledULXC}); err != nil {
				return out, err
			}
		}
		if agentID != "" && out.SettledFeeULXC != 0 {
			if err := postFeeEntry(ctx, tx, workspaceID, agentID, out.SettledFeeULXC, bps, debitKey, meta.RequestedModel); err != nil {
				return out, err
			}
		}
	}
	// B27.6: the pre-serve debit, like a hold, did not touch backing, so the whole charge consumes it here, as a
	// settled reservation does, against the balance with the charge undone — and reports the royalty basis. Its
	// platform fee consumes backing after it, and funds no royalty.
	if charge > 0 {
		bal, _, _, err := readLXCBalance(ctx, tx, workspaceID)
		if err != nil {
			return out, err
		}
		fee := estFee + out.SettledFeeULXC
		fromCash, err := consumeCashBacked(ctx, tx, workspaceID, bal+charge+fee, charge)
		if err != nil {
			return out, err
		}
		if _, err := consumeCashBacked(ctx, tx, workspaceID, bal+fee, fee); err != nil {
			return out, err
		}
		if out.CashBackedULXC, err = royaltyBacked(ctx, tx, workspaceID, charge, fromCash); err != nil {
			return out, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_debit_settlements SET agent_id = $2, settled_ulxc = $3, written_off_ulxc = $4
		WHERE debit_key = $1`, debitKey, nullIfEmpty(agentID), out.SettledULXC, out.WrittenOffULXC); err != nil {
		return out, fmt.Errorf("economy: record agent debit settlement: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentDebitSettlement{}, fmt.Errorf("economy: commit agent debit settle: %w", err)
	}
	return out, nil
}

// agentAllowance is how much of want µLXC beyond a question's estimate the agent's limit still allows, with
// the agent locked: its per-request and period limits (B19.2, B28.300) and its daily cap on the question's
// model (B28.301), counted with the estimate already spent, and its balance — topped up from its company's
// credit line where the spend path would draw it (B22.4).
func agentAllowance(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, estimate, want int64, ref, model string) (int64, error) {
	r, err := agentRulesInForce(ctx, tx, agentID, time.Now()) // with its boosts in force now (B28.308)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("economy: agent rules: %w", err)
	}
	if r.MaxPerRequestULXC > 0 {
		want = min(want, max(r.MaxPerRequestULXC-estimate, 0))
	}
	loc, err := r.location()
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	if limit, since := r.modelDailyLimit(model, now, loc); limit > 0 {
		spent, err := agentModelSpentSince(ctx, tx, workspaceID, agentID, model, since)
		if err != nil {
			return 0, fmt.Errorf("economy: agent spend on the model so far: %w", err)
		}
		want = min(want, max(limit-spent, 0))
	}
	for _, limit := range r.periodLimits(now, loc) {
		if limit.ulxc == 0 {
			continue
		}
		spent, err := agentSpentSince(ctx, tx, workspaceID, agentID, limit.since)
		if err != nil {
			return 0, fmt.Errorf("economy: agent spend so far: %w", err)
		}
		want = min(want, max(limit.ulxc-spent, 0))
	}
	if want == 0 {
		return 0, nil
	}
	bal, err := accountBalance(ctx, tx, workspaceID, agentAccount(agentID))
	if err != nil {
		return 0, err
	}
	if bal < want {
		drew, err := drawCreditLine(ctx, tx, workspaceID, agentID, want-bal, ref)
		if err != nil {
			return 0, err
		}
		if drew {
			bal = want
		}
	}
	return min(want, max(bal, 0)), nil
}
