package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// agent_payments.go — B19.3: ONE COMPANY'S AGENTS PAY EACH OTHER, INSIDE THE CLOSED LOOP.
//
// A payment is ONE entry of two postings: agent:<payer> −amount, agent:<payee> +amount. The LXC never
// leaves the workspace — lxc_balances and lxc_ledger do not move, and neither do the 'workspace' and
// 'spend' accounts — so it cannot mint, redeem or reach another company. The payer's rules (B19.2) judge
// it as spending: its active hours, its limits per payment, day and month, and its approval amount.

// ErrSameAgent: an agent cannot pay itself.
var ErrSameAgent = errors.New("economy: an agent cannot pay itself")

// AgentPayment is one payment between two of a workspace's agents.
type AgentPayment struct {
	EntryID         string `json:"entry_id"`
	FromAgentID     string `json:"from_agent_id"`
	ToAgentID       string `json:"to_agent_id"`
	AmountULXC      int64  `json:"amount_ulxc"`
	FromBalanceULXC int64  `json:"from_balance_ulxc"`
	ToBalanceULXC   int64  `json:"to_balance_ulxc"`
	Memo            string `json:"memo,omitempty"`
}

// AgentOfKey returns the agent a key is attached to, and its workspace; ErrAgentNotFound for a key
// attached to none.
func (s *DualTokenStore) AgentOfKey(ctx context.Context, scopedKeyID string) (agentID, workspaceID string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT a.id, a.workspace_id FROM agent_account_keys k JOIN agent_accounts a ON a.id = k.agent_id
		WHERE k.scoped_key_id = $1`, scopedKeyID).Scan(&agentID, &workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrAgentNotFound
	}
	return agentID, workspaceID, err
}

// PayAgent moves amount µLXC from one of workspaceID's agents to another.
func (s *DualTokenStore) PayAgent(ctx context.Context, workspaceID, fromAgentID, toAgentID string, amount int64, memo string) (AgentPayment, error) {
	pay := AgentPayment{FromAgentID: fromAgentID, ToAgentID: toAgentID, AmountULXC: amount, Memo: memo}
	if amount <= 0 {
		return pay, errors.New("economy: the amount must be positive")
	}
	if fromAgentID == toAgentID {
		return pay, ErrSameAgent
	}
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Fingerprint: paymentFingerprint(fromAgentID, toAgentID, amount, memo)})

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return pay, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if pay, err = payAgentTx(ctx, tx, workspaceID, pay, uuid.New()); err != nil {
		return pay, s.refusedMovement(ctx, tx, err)
	}
	return pay, tx.Commit(ctx)
}

// paymentFingerprint is what an approval of a payment is for: the payer, the payee, the amount and the memo.
func paymentFingerprint(fromAgentID, toAgentID string, amount int64, memo string) string {
	fp := sha256.Sum256([]byte("payment\x00" + fromAgentID + "\x00" + toAgentID + "\x00" + strconv.FormatInt(amount, 10) + "\x00" + memo))
	return hex.EncodeToString(fp[:])
}

// payAgentTx posts pay as entry inside tx, judged by the payer's rules. Its refusals are returned as
// they are; PayAgent files the approval one needs.
func payAgentTx(ctx context.Context, tx pgx.Tx, workspaceID string, pay AgentPayment, entry uuid.UUID) (AgentPayment, error) {
	fromAgentID, toAgentID, amount, memo := pay.FromAgentID, pay.ToAgentID, pay.AmountULXC, pay.Memo
	// Both agents' rows, in id order, so two opposite payments cannot deadlock.
	first, second := fromAgentID, toAgentID
	if second < first {
		first, second = second, first
	}
	for _, id := range []string{first, second} {
		if err := lockAgent(ctx, tx, workspaceID, id); err != nil {
			return pay, err
		}
	}
	bal, err := accountBalance(ctx, tx, workspaceID, agentAccount(fromAgentID))
	if err != nil {
		return pay, err
	}
	if bal < amount {
		return pay, fmt.Errorf("%w: the agent holds %d µLXC", ErrAgentFunds, bal)
	}
	pay.EntryID = entry.String()
	if err := enforceAgentRules(ctx, tx, workspaceID, fromAgentID, amount, pay.EntryID); err != nil {
		return pay, err
	}
	for _, l := range []leg{{agentAccount(fromAgentID), -amount}, {agentAccount(toAgentID), amount}} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_postings (entry_id, workspace_id, account, amount_ulxc, kind, ref) VALUES ($1, $2, $3, $4, 'pay', $5)`,
			entry, workspaceID, l.account, l.amount, memo); err != nil {
			return pay, fmt.Errorf("economy: post payment: %w", err)
		}
	}
	if pay.ToBalanceULXC, err = accountBalance(ctx, tx, workspaceID, agentAccount(toAgentID)); err != nil {
		return pay, err
	}
	pay.FromBalanceULXC = bal - amount
	return pay, nil
}

// AgentStatementLine is one posting on an agent's account, with what it moved against and the balance
// it left.
type AgentStatementLine struct {
	EntryID          string    `json:"entry_id"`
	Kind             string    `json:"kind"` // fund | withdraw | spend | hold | settle | release | pay
	AmountULXC       int64     `json:"amount_ulxc"`
	Counterparty     string    `json:"counterparty"`  // workspace | spend | agent:<id>
	Ref              string    `json:"ref,omitempty"` // a payment's memo, or the request it paid for
	BalanceAfterULXC int64     `json:"balance_after_ulxc"`
	At               time.Time `json:"at"`
}

// AgentStatement reads an agent's account, newest first, at most limit lines.
func (s *DualTokenStore) AgentStatement(ctx context.Context, workspaceID, agentID string, limit int) ([]AgentStatementLine, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("economy: agent statement: %w", err)
	}
	if !exists {
		return nil, ErrAgentNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT entry_id::text, kind, amount_ulxc, counterparty, ref, balance_after, created_at FROM (
		  SELECT p.id, p.entry_id, p.kind, p.amount_ulxc, p.ref, p.created_at,
		         COALESCE((SELECT o.account FROM agent_postings o WHERE o.entry_id = p.entry_id AND o.id <> p.id LIMIT 1), '') AS counterparty,
		         sum(p.amount_ulxc) OVER (ORDER BY p.id)::bigint AS balance_after
		    FROM agent_postings p WHERE p.workspace_id = $1 AND p.account = $2) lines
		ORDER BY id DESC LIMIT $3`, workspaceID, agentAccount(agentID), limit)
	if err != nil {
		return nil, fmt.Errorf("economy: agent statement: %w", err)
	}
	defer rows.Close()
	out := []AgentStatementLine{}
	for rows.Next() {
		var l AgentStatementLine
		if err := rows.Scan(&l.EntryID, &l.Kind, &l.AmountULXC, &l.Counterparty, &l.Ref, &l.BalanceAfterULXC, &l.At); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
