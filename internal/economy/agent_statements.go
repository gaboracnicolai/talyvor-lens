package economy

import (
	"context"
	"fmt"
	"time"
)

// agent_statements.go — B19.5: STATEMENTS AN ENTERPRISE CAN AUDIT.
//
// A statement covers the half-open period [From, To) and is built from agent_postings alone: the
// append-only, double-entry record every agent movement is written to (0140). Nothing it reads can be
// changed or deleted, so asking for the same closed period again re-derives the same lines and totals,
// whatever was posted since. Each line carries its posting's id and entry id — the row of the audit log
// it came from — and a spend, hold, settle or release its request or reservation id, the same id its
// lxc_ledger row carries.
//
// A posting belongs to the period its created_at falls in (the time its transaction began), and lines
// are ordered by (created_at, id), so a line's balance is its account's opening balance plus every line
// before it. A period that has not ended yet is not closed: a movement recorded later can still fall
// inside it.

// StatementLine is one posting in a statement.
type StatementLine struct {
	PostingID        int64     `json:"posting_id"` // agent_postings.id
	EntryID          string    `json:"entry_id"`
	At               time.Time `json:"at"`
	Account          string    `json:"account"` // workspace | spend | agent:<id>
	Kind             string    `json:"kind"`    // fund | withdraw | spend | hold | settle | release | pay | card
	AmountULXC       int64     `json:"amount_ulxc"`
	Counterparty     string    `json:"counterparty"`
	Ref              string    `json:"ref,omitempty"`
	BalanceAfterULXC int64     `json:"balance_after_ulxc"`
}

// StatementAccount is one account's totals over the period: Opening + In − Out = Closing.
type StatementAccount struct {
	Account     string `json:"account"`
	OpeningULXC int64  `json:"opening_ulxc"`
	InULXC      int64  `json:"in_ulxc"`
	OutULXC     int64  `json:"out_ulxc"`
	ClosingULXC int64  `json:"closing_ulxc"`
}

// Statement is an agent's account, or every agent wallet a workspace has, over [From, To).
type Statement struct {
	WorkspaceID string             `json:"workspace_id"`
	AgentID     string             `json:"agent_id,omitempty"`
	From        time.Time          `json:"from"`
	To          time.Time          `json:"to"`
	Accounts    []StatementAccount `json:"accounts"`
	Lines       []StatementLine    `json:"lines"`
}

// AgentPeriodStatement is one agent's statement for [from, to).
func (s *DualTokenStore) AgentPeriodStatement(ctx context.Context, workspaceID, agentID string, from, to time.Time) (Statement, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return Statement{}, fmt.Errorf("economy: agent statement: %w", err)
	}
	if !exists {
		return Statement{}, ErrAgentNotFound
	}
	st, err := s.periodStatement(ctx, workspaceID, agentAccount(agentID), from, to)
	st.AgentID = agentID
	if err == nil && len(st.Accounts) == 0 { // nothing ever posted: the account still has a (zero) line
		st.Accounts = []StatementAccount{{Account: agentAccount(agentID)}}
	}
	return st, err
}

// WorkspaceAgentStatement is the statement of every agent wallet in a workspace — the
// workspace's side, each agent, and what they spent — for [from, to). Every entry sums to zero, so the
// accounts' movements over any period do too.
func (s *DualTokenStore) WorkspaceAgentStatement(ctx context.Context, workspaceID string, from, to time.Time) (Statement, error) {
	return s.periodStatement(ctx, workspaceID, "", from, to)
}

// periodStatement reads [from, to) for one account, or for every account when account is "".
func (s *DualTokenStore) periodStatement(ctx context.Context, workspaceID, account string, from, to time.Time) (Statement, error) {
	st := Statement{WorkspaceID: workspaceID, From: from, To: to, Accounts: []StatementAccount{}, Lines: []StatementLine{}}
	rows, err := s.pool.Query(ctx, `
		SELECT account,
		       COALESCE(sum(amount_ulxc) FILTER (WHERE created_at < $3), 0)::bigint,
		       COALESCE(sum(amount_ulxc) FILTER (WHERE created_at >= $3 AND amount_ulxc > 0), 0)::bigint,
		       COALESCE(-sum(amount_ulxc) FILTER (WHERE created_at >= $3 AND amount_ulxc < 0), 0)::bigint
		  FROM agent_postings
		 WHERE workspace_id = $1 AND ($2 = '' OR account = $2) AND created_at < $4
		 GROUP BY account ORDER BY account`, workspaceID, account, from, to)
	if err != nil {
		return st, fmt.Errorf("economy: statement totals: %w", err)
	}
	opening := map[string]int64{}
	for rows.Next() {
		var a StatementAccount
		if err := rows.Scan(&a.Account, &a.OpeningULXC, &a.InULXC, &a.OutULXC); err != nil {
			rows.Close()
			return st, err
		}
		a.ClosingULXC = a.OpeningULXC + a.InULXC - a.OutULXC
		opening[a.Account] = a.OpeningULXC
		st.Accounts = append(st.Accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("economy: statement totals: %w", err)
	}

	rows, err = s.pool.Query(ctx, `
		SELECT p.id, p.entry_id::text, p.created_at, p.account, p.kind, p.amount_ulxc,
		       COALESCE((SELECT o.account FROM agent_postings o WHERE o.entry_id = p.entry_id AND o.id <> p.id ORDER BY o.id LIMIT 1), ''),
		       p.ref,
		       sum(p.amount_ulxc) OVER (PARTITION BY p.account ORDER BY p.created_at, p.id)::bigint
		  FROM agent_postings p
		 WHERE p.workspace_id = $1 AND ($2 = '' OR p.account = $2) AND p.created_at >= $3 AND p.created_at < $4
		 ORDER BY p.created_at, p.id`, workspaceID, account, from, to)
	if err != nil {
		return st, fmt.Errorf("economy: statement lines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l StatementLine
		var running int64
		if err := rows.Scan(&l.PostingID, &l.EntryID, &l.At, &l.Account, &l.Kind, &l.AmountULXC, &l.Counterparty, &l.Ref, &running); err != nil {
			return st, err
		}
		l.At = l.At.UTC()
		l.BalanceAfterULXC = opening[l.Account] + running
		st.Lines = append(st.Lines, l)
	}
	return st, rows.Err()
}
