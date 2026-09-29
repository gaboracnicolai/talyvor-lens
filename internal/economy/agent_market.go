package economy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/jackc/pgx/v5"
)

// agent_market.go — B20.2: AN AGENT USES A PAID MARKETPLACE LISTING.
//
// The charge lands on its company's monthly marketplace bill, not on the agent's balance, but the agent's
// rules judge it exactly as they judge a payment from it: paused, its hours, its limit per request, its
// daily and monthly limits (which count what it spent on listings, see enforceAgentRules), the listings it
// may use (B19.14), and the approval amount — a use above it files an approval, and once approved that use
// goes through once.

// JudgeAgentPurchase judges agentID's use of listingID, costing amount µLXC (0 for a free one), against
// its rules and, if they let it through, runs record in the same transaction — so the next purchase's
// limits count this one. what names the purchase for an approval (the listing, its version and price); a
// refusal that needs an approval files it.
func (s *DualTokenStore) JudgeAgentPurchase(ctx context.Context, workspaceID, agentID, listingID string, amount int64, what string, record func(pgx.Tx) error) error {
	fp := sha256.Sum256([]byte("purchase\x00" + agentID + "\x00" + what))
	ctx = WithAgentRequest(ctx, AgentRequest{Payment: true, Listing: listingID, Fingerprint: hex.EncodeToString(fp[:]),
		Payee: Payee{Kind: "listing", ID: listingID}, Memo: what})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockAgent(ctx, tx, workspaceID, agentID); err != nil {
		return err
	}
	if err := enforceAgentRules(ctx, tx, workspaceID, agentID, amount, what); err != nil {
		return s.refusedMovement(ctx, tx, err)
	}
	if err := record(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
