package market

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/workspace"
)

// schedule.go — B19.17: AN AGENT'S SCHEDULE PAYS A MARKETPLACE LISTING.
//
// A schedule (economy.agent_payment_schedules) may pay a listing every hour, day, week or month. Each tick
// is one billed use of the listing (market_uses, stamped at the tick), judged by the paying agent's rules
// and recorded in the tick's own transaction — so a tick is billed once, across a restart — and metered
// onto the company's monthly marketplace bill by MeterPending, like any use Stripe has not yet accepted.
// A tick pays; it does not run the listing.

func lxc(ulxc int64) string { return strconv.FormatFloat(float64(ulxc)/1e6, 'f', -1, 64) }

// ListingPrice is what one use of listingID costs buyerWorkspaceID, or a refusal saying why a schedule of
// that workspace cannot pay it.
func (s *Store) ListingPrice(ctx context.Context, buyerWorkspaceID, listingID string) (int64, string, error) {
	l, refusal, err := s.payable(ctx, s.pool, buyerWorkspaceID, listingID)
	if err != nil || refusal != "" {
		return 0, refusal, err
	}
	return l.PricePerUseULXC, "", nil
}

// ChargeScheduledListing records, in tx, one billed use of listingID by agentID at the tick at, once judge
// lets its price through and never above maxULXC.
func (s *Store) ChargeScheduledListing(ctx context.Context, tx pgx.Tx, buyerWorkspaceID, agentID, listingID string, maxULXC int64,
	at time.Time, judge func(price int64) error) (string, string, error) {
	l, refusal, err := s.payable(ctx, tx, buyerWorkspaceID, listingID)
	if err != nil || refusal != "" {
		return "", refusal, err
	}
	if l.PricePerUseULXC > maxULXC {
		return "", fmt.Sprintf("the listing now costs %s LXC a use, more than this schedule pays (%s LXC)", lxc(l.PricePerUseULXC), lxc(maxULXC)), nil
	}
	if err := judge(l.PricePerUseULXC); err != nil {
		return "", "", err
	}
	id := "use_" + uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO market_uses (id, listing_id, version, seller_workspace_id, buyer_workspace_id, agent_id, price_ulxc, charge, used_at, ran_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'billed', $8, $8)`,
		id, l.ID, l.LatestVersion, l.WorkspaceID, buyerWorkspaceID, agentID, l.PricePerUseULXC, at); err != nil {
		return "", "", fmt.Errorf("market: record scheduled use: %w", err)
	}
	return id, "", nil
}

// payable reads a listing a schedule of buyerWorkspaceID may pay, or says why it may not.
func (s *Store) payable(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, buyer, listingID string) (Listing, string, error) {
	l, err := scanListing(q.QueryRow(ctx, `SELECT `+listingColumns+` FROM market_listings WHERE id = $1`, listingID))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hidden(l, buyer)) {
		return l, "there is no such listing", nil
	}
	if err != nil {
		return l, "", fmt.Errorf("market: listing: %w", err)
	}
	if l.ReviewStatus == ReviewTakenDown {
		return l, "the listing was taken down", nil
	}
	switch {
	case l.WorkspaceID == buyer:
		return l, "a schedule cannot pay the workspace's own listing", nil
	case l.PricePerUseULXC == 0:
		return l, "the listing is free, so there is nothing to pay", nil
	}
	if err := workspace.CheckMoneyWall(ctx, q, buyer, l.WorkspaceID); errors.Is(err, workspace.ErrMoneyWall) {
		return l, err.Error(), nil
	} else if err != nil {
		return l, "", err
	}
	linked, err := s.linked(ctx, l.WorkspaceID, buyer)
	if err != nil {
		return l, "", err
	}
	if linked {
		return l, "the workspace and the listing's seller share a card or an owner, and a seller cannot pay themselves", nil
	}
	return l, "", nil
}
