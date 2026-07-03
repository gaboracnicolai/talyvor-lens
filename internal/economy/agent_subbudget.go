package economy

import (
	"context"
	"errors"
	"fmt"
)

// agent_subbudget.go — F4-capstone step A: the per-scoped-key LXC sub-budget + the EXACTLY-ONCE agent debit.
// This is the substrate the closed-loop allocator (a later step) sits on. It MINTS NOTHING — it bounds and
// records SPENDING of already-existing LXC (workspace↔Talyvor pool), crediting no ledger and adding no mint
// type. CENTRAL-COUNTERPARTY + CLOSED-LOOP: LXC is debited from the workspace's lxc_balances via the SAME
// internals SpendLXC uses (readLXCBalance/insertLXCLedger/writeLXCBalance) — this file NEVER touches
// LedgerStore.Transfer (LENS P2P) or the marketplace (asserted by agent_subbudget_noloop_test.go).

// ErrSubBudgetExceeded is returned when a debit would push the agent's spent_lxc past its ceiling. The whole
// transaction rolls back — no claim, no debit — so the request_id stays retriable (e.g. after a ceiling raise).
var ErrSubBudgetExceeded = errors.New("economy: agent LXC sub-budget ceiling exceeded")

// DefaultAgentCeilingLXC is the ceiling applied to a scoped key with no explicit ceiling — 50 LXC ($5 at the
// 1 LXC = $0.10 peg). Baked here per the owner's step-A instruction (the capstone ships armed with a test
// ceiling). A funded agent spends up to this; an unfunded one (zero LXC balance) spends nothing.
const DefaultAgentCeilingLXC = 50.0

// SetAgentCeiling upserts a scoped key's LXC ceiling (preserving spent_lxc). This is how an operator sets a
// per-agent cap other than the default.
func (s *DualTokenStore) SetAgentCeiling(ctx context.Context, scopedKeyID, workspaceID string, ceilingLXC float64) error {
	if s == nil || s.pool == nil {
		return nil
	}
	if scopedKeyID == "" || workspaceID == "" {
		return errors.New("economy: SetAgentCeiling requires scoped_key_id + workspace_id")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, $3, 0)
		 ON CONFLICT (scoped_key_id) DO UPDATE SET ceiling_lxc = EXCLUDED.ceiling_lxc, updated_at = now()`,
		scopedKeyID, workspaceID, ceilingLXC)
	if err != nil {
		return fmt.Errorf("economy: set agent ceiling: %w", err)
	}
	return nil
}

// SpendLXCForAgent debits lxcAmount from the workspace's LXC balance on behalf of a scoped key (the "agent"),
// EXACTLY ONCE per requestID and only within the agent's remaining sub-budget. All of {claim, ceiling check,
// balance debit, spent bump} happen in ONE transaction — a claim without a debit, or a debit without a
// claim, is the double-spend bug this method exists to prevent.
//
// Returns nil on a fresh successful debit AND on an idempotent replay (a requestID already claimed ⇒ nothing
// debited). Returns ErrSubBudgetExceeded (ceiling) or ErrInsufficientLXC (balance) on a rejected debit —
// both roll the whole tx back (no orphan claim, retriable).
func (s *DualTokenStore) SpendLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, requestID string, lxcAmount float64, description string) error {
	if lxcAmount <= 0 {
		return errors.New("economy: agent spend amount must be positive")
	}
	if scopedKeyID == "" || workspaceID == "" || requestID == "" {
		return errors.New("economy: agent spend requires scoped_key_id, workspace_id, request_id")
	}
	if s == nil || s.pool == nil {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: begin agent spend: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// (1) EXACTLY-ONCE claim. ON CONFLICT DO NOTHING ⇒ 0 rows means this requestID already succeeded — an
	// idempotent replay: debit NOTHING, return nil. The claim is committed in THIS tx (below) only on success.
	tag, err := tx.Exec(ctx,
		`INSERT INTO lxc_spend_claims (request_id, scoped_key_id, lxc_amount) VALUES ($1, $2, $3)
		 ON CONFLICT (request_id) DO NOTHING`, requestID, scopedKeyID, lxcAmount)
	if err != nil {
		return fmt.Errorf("economy: spend claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // idempotent replay — already debited under this request_id
	}

	// (2) Ensure + LOCK the sub-budget row (FOR UPDATE serializes concurrent debits for this agent).
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, $3, 0) ON CONFLICT (scoped_key_id) DO NOTHING`,
		scopedKeyID, workspaceID, DefaultAgentCeilingLXC); err != nil {
		return fmt.Errorf("economy: ensure sub-budget: %w", err)
	}
	var ceiling, spent float64
	if err := tx.QueryRow(ctx,
		`SELECT ceiling_lxc, spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1 FOR UPDATE`,
		scopedKeyID).Scan(&ceiling, &spent); err != nil {
		return fmt.Errorf("economy: read sub-budget: %w", err)
	}

	// (3) CEILING check — reject (rollback ⇒ no orphan claim) if this debit would exceed remaining.
	if ceiling-spent < lxcAmount {
		return ErrSubBudgetExceeded
	}

	// (4) Debit the workspace LXC via the SAME internals SpendLXC uses (no duplication of the balance path).
	bal, minted, wsSpent, err := readLXCBalance(ctx, tx, workspaceID) // FOR UPDATE
	if err != nil {
		return err
	}
	if bal < lxcAmount {
		return ErrInsufficientLXC // rollback ⇒ no orphan claim; retriable after funding
	}
	newBal := roundTo(bal-lxcAmount, 6)
	if err := insertLXCLedger(ctx, tx, workspaceID, -lxcAmount, newBal, LXCTypeSpend, description, nil); err != nil {
		return err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, newBal, minted, wsSpent+lxcAmount); err != nil {
		return err
	}

	// (5) Bump the agent's spent_lxc (monotonic) — atomic with the debit + the claim.
	if _, err := tx.Exec(ctx,
		`UPDATE agent_lxc_subbudgets SET spent_lxc = spent_lxc + $2, updated_at = now() WHERE scoped_key_id = $1`,
		scopedKeyID, lxcAmount); err != nil {
		return fmt.Errorf("economy: bump spent: %w", err)
	}

	return tx.Commit(ctx)
}
