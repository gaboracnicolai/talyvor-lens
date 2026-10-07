package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/plans"
)

// room_wallet.go — B32.32: A ROOM'S WALLET.
//
// A room's wallet is an agent account of kind room in its owner's workspace (CreateRoomAgentTx). Its owner funds it and
// sets its rules as any agent's, and its approvals reach the owner's; its monthly limit is the room's budget, at most
// what the owner's plan allows a room (rooms_plan_limits' room_budget_max_usd, internal/rooms). Every way the monthly
// limit changes — the rules saved, a version rolled back to, a boost — is judged here.

// RoomBudgets judges a room wallet's monthly limit against the room owner's plan. internal/rooms implements it; economy
// cannot import rooms, which imports the marketplace, which imports economy.
type RoomBudgets interface {
	// CheckRoomBudget refuses monthlyULXC — 0 is no monthly limit — when workspaceID's plan does not allow a room it.
	CheckRoomBudget(ctx context.Context, q plans.Querier, workspaceID string, monthlyULXC int64) error
}

// SetRoomBudgets sets what judges a room wallet's monthly limit. Unset, a room wallet's rules cannot be saved.
func (s *DualTokenStore) SetRoomBudgets(b RoomBudgets) { s.roomBudgets = b }

// checkRoomBudget refuses monthlyULXC for agentID when it is a room's wallet and the room owner's plan does not allow it.
// Any other agent's monthly limit is its owner's to set.
func (s *DualTokenStore) checkRoomBudget(ctx context.Context, q plans.Querier, workspaceID, agentID string, monthlyULXC int64) error {
	var kind string
	err := q.QueryRow(ctx, `SELECT kind FROM agent_accounts WHERE id = $1 AND workspace_id = $2`, agentID, workspaceID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAgentNotFound
	}
	if err != nil {
		return fmt.Errorf("economy: agent kind: %w", err)
	}
	if kind != AgentKindRoom {
		return nil
	}
	if s.roomBudgets == nil {
		return errors.New("economy: a room's budget cannot be set: room budgets are not configured")
	}
	return s.roomBudgets.CheckRoomBudget(ctx, q, workspaceID, monthlyULXC)
}

// AgentBudget is a room's wallet as the room's members see it (B32.32): its balance, its monthly limit — the room's
// budget — and what it has spent of it this month, its limit per request and the amount above which a spend waits for
// its owner's approval. A zero limit is none.
type AgentBudget struct {
	BalanceULXC        int64 `json:"balance_ulxc"`
	MonthlyLimitULXC   int64 `json:"monthly_limit_ulxc"`
	SpentThisMonthULXC int64 `json:"spent_this_month_ulxc"`
	MaxPerRequestULXC  int64 `json:"max_per_request_ulxc"`
	ApprovalAboveULXC  int64 `json:"approval_above_ulxc"`
}

// ReadAgentBudget reads agentID's budget in tx as of now: its rules in force then, boosts included, and what it has
// spent since its month began in their timezone, as its monthly limit counts it.
func ReadAgentBudget(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, now time.Time) (AgentBudget, error) {
	var b AgentBudget
	r, err := agentRulesInForce(ctx, tx, agentID, now)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return b, fmt.Errorf("economy: agent budget: %w", err)
	}
	loc, err := r.location()
	if err != nil {
		return b, fmt.Errorf("economy: agent budget: the agent's timezone %q cannot be read", r.Timezone)
	}
	b.MonthlyLimitULXC, b.MaxPerRequestULXC, b.ApprovalAboveULXC = r.MonthlyLimitULXC, r.MaxPerRequestULXC, r.ApprovalAboveULXC
	local := now.In(loc)
	if b.SpentThisMonthULXC, err = agentSpentSince(ctx, tx, workspaceID, agentID,
		time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)); err != nil {
		return b, fmt.Errorf("economy: agent budget: %w", err)
	}
	if b.BalanceULXC, err = accountBalance(ctx, tx, workspaceID, agentAccount(agentID)); err != nil {
		return b, fmt.Errorf("economy: agent budget: %w", err)
	}
	return b, nil
}
