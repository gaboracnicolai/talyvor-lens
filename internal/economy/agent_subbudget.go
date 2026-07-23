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
// 1 LXC = $0.10 peg), in µLXC (SEC-2). Baked here per the owner's step-A instruction (the capstone ships
// armed with a test ceiling). A funded agent spends up to this; an unfunded one (zero LXC balance) spends nothing.
const DefaultAgentCeilingLXC int64 = 50_000_000

// SetAgentCeiling upserts a scoped key's LXC ceiling in µLXC (preserving spent_lxc). This is how an operator
// sets a per-agent cap other than the default.
func (s *DualTokenStore) SetAgentCeiling(ctx context.Context, scopedKeyID, workspaceID string, ceilingLXC int64) error {
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

// AgentDebitMeta is the NON-CONTENT metadata stamped on an agent-allocator lxc_ledger debit row. It
// exists so the ledger is READABLE (per-model spend derivable) and a money row JOINS to its usage row —
// without lxc_ledger, an append-only + immutable financial record (migration 0055), ever becoming a
// content record. It carries EXACTLY two scalars and structurally cannot carry prompt text, a hash, or an
// embedding (there is no field for content):
//
//   - RequestedModel: the model the charge was ESTIMATED on. The debit is PRE-SERVE / PRE-ROUTING, so this
//     is the REQUESTED model, NOT necessarily the one that served (token_events records the served model).
//     Named "requested_model" on the row so a UI cannot imply it was the serving model.
//   - RequestID: the token_events request_id, so the money row joins to its usage row (model served, real
//     token counts) instead of duplicating any of that here.
type AgentDebitMeta struct {
	RequestedModel string
	RequestID      string
}

// toMap renders the two scalars as the lxc_ledger metadata document, OMITTING an empty scalar so a row
// carries no empty-string noise. The economy layer owns this shape: a caller supplies only these two typed
// strings, never a free-form map, so no content can be injected into a money row.
func (m AgentDebitMeta) toMap() map[string]interface{} {
	out := map[string]interface{}{}
	if m.RequestedModel != "" {
		out["requested_model"] = m.RequestedModel
	}
	if m.RequestID != "" {
		out["request_id"] = m.RequestID
	}
	return out
}

// SpendLXCForAgent debits lxcAmount from the workspace's LXC balance on behalf of a scoped key (the "agent"),
// EXACTLY ONCE per requestID and only within the agent's remaining sub-budget. All of {claim, ceiling check,
// balance debit, spent bump} happen in ONE transaction — a claim without a debit, or a debit without a
// claim, is the double-spend bug this method exists to prevent. `meta` stamps the debit row with the
// requested model + token_events request_id (non-content; see AgentDebitMeta) so the ledger is readable.
//
// Returns nil on a fresh successful debit AND on an idempotent replay (a requestID already claimed ⇒ nothing
// debited). Returns ErrSubBudgetExceeded (ceiling) or ErrInsufficientLXC (balance) on a rejected debit —
// both roll the whole tx back (no orphan claim, retriable).
func (s *DualTokenStore) SpendLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, requestID string, lxcAmount int64, description string, meta AgentDebitMeta) error {
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
	var ceiling, spent int64 // µLXC
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
	newBal := bal - lxcAmount // exact integer µLXC
	// Stamp the non-content metadata (requested model + token_events request_id) so the ledger is readable
	// and joins to token_events. meta.toMap() carries ONLY those two scalars — never content (0055 immutable).
	if err := insertLXCLedger(ctx, tx, workspaceID, -lxcAmount, newBal, LXCTypeSpend, description, meta.toMap()); err != nil {
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
