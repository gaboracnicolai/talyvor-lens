package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MODEL 2, STEP 2 — THE ALLOWANCE LEDGER. W4.6.1.
//
// Step 1 recorded that a workspace PAYS. This records what paying entitles them to,
// and the ceiling that makes the product priceable.
//
//	phi = F/D < 1   — the allowance D is worth more than the fee F. That is the offer.
//	HARD CAP        — a subscriber cannot consume D + 1. That is what makes it safe.
//	worst case      — EXACTLY D per subscriber per period, so it can be priced rather
//	                  than hoped for.
//
// ⚠ F AND D ARE NICOLAI'S AND ARE HARDCODED NOWHERE. D arrives as configuration
// (LENS_SUBSCRIPTION_ALLOWANCE_ULXC) and DEFAULTS TO ZERO, which means "no allowance
// configured" and leaves the whole mechanism inert: no grant row is written, and
// `Consume` returns "nothing covered" for every request. An undecided price should
// look like an undecided price, not like a default someone will mistake for a
// decision. F is the Stripe Price from step 1; this file never sees it.
//
// ⚠ WHAT THIS FILE DOES NOT DO: it does not gate serving. `Consume` reports how much
// of a cost the allowance covered; deciding what to do with the remainder is the
// caller's. The existing LXC admission gate reads a prepaid balance and is behind its
// own default-off flag; making it read allowance-then-prepaid is a change to the
// SERVING path and belongs in its own merge with its own controls. Wiring a money
// gate as a side effect of building a ledger is how a serving regression ships.

// ErrNoAllowanceConfigured is returned by Grant when D is zero — the default. It is a
// CONFIGURATION state, not a failure: a deployment that has not priced the allowance
// simply has none.
var ErrNoAllowanceConfigured = errors.New("billing: no subscription allowance configured (LENS_SUBSCRIPTION_ALLOWANCE_ULXC)")

// Allowance is one period's grant and what is left of it.
type Allowance struct {
	WorkspaceID    string    `json:"workspace_id"`
	SubscriptionID string    `json:"subscription_id"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
	GrantedULXC    int64     `json:"granted_ulxc"`
	ConsumedULXC   int64     `json:"consumed_ulxc"`
	RemainingULXC  int64     `json:"remaining_ulxc"`
}

// WithAllowance sets D, in µLXC, for grants made by this Service. Zero (the default)
// leaves the mechanism inert. Separate from New for the same reason WithSubscriptions
// is: every existing caller and test double stays exactly as it was.
func (s *Service) WithAllowance(grantULXC int64) *Service {
	s.allowanceULXC = grantULXC
	return s
}

// Grant creates the allowance row for one billing period. Idempotent by the unique
// index on (subscription, period_start).
//
// ⚠ IDEMPOTENCY HERE IS NOT TIDINESS, IT IS THE WORST-CASE BOUND. Stripe redelivers;
// a renewal processed twice that granted twice would hand one subscriber 2D for one
// fee, which is precisely the thing "worst case per subscriber is EXACTLY D" claims
// cannot happen. ON CONFLICT DO NOTHING makes the second grant a no-op, and the
// caller learns nothing new happened from `created`.
func (s *Service) Grant(ctx context.Context, workspaceID, subscriptionID string, periodStart, periodEnd time.Time) (created bool, err error) {
	if s.allowanceULXC <= 0 {
		return false, ErrNoAllowanceConfigured
	}
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO subscription_allowance
			(workspace_id, stripe_subscription_id, period_start, period_end, granted_ulxc)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (stripe_subscription_id, period_start) DO NOTHING`,
		workspaceID, subscriptionID, periodStart, periodEnd, s.allowanceULXC)
	if err != nil {
		return false, fmt.Errorf("billing: grant allowance: %w", err)
	}
	return ct.RowsAffected() == 1, nil
}

// CurrentAllowance returns the grant covering `at`, or nil when there is none.
func (s *Service) CurrentAllowance(ctx context.Context, workspaceID string, at time.Time) (*Allowance, error) {
	var a Allowance
	err := s.pool.QueryRow(ctx, `
		SELECT workspace_id, stripe_subscription_id, period_start, period_end, granted_ulxc, consumed_ulxc
		FROM subscription_allowance
		WHERE workspace_id = $1 AND period_start <= $2 AND period_end > $2
		ORDER BY period_start DESC LIMIT 1`, workspaceID, at).
		Scan(&a.WorkspaceID, &a.SubscriptionID, &a.PeriodStart, &a.PeriodEnd, &a.GrantedULXC, &a.ConsumedULXC)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("billing: read allowance: %w", err)
	}
	a.RemainingULXC = a.GrantedULXC - a.ConsumedULXC
	return &a, nil
}

// Consume draws `costULXC` against the current allowance and reports how much it
// covered. The remainder is the caller's problem — prepaid LXC at the metered rate.
//
// ⚠ IT CLAMPS, IT DOES NOT REFUSE, AND THAT IS THE WHOLE DESIGN. A cost larger than
// the remaining allowance covers what is left and returns the rest as uncovered. The
// alternatives are both wrong:
//
//   - REFUSING the whole request the moment it would overflow strands the last of an
//     allowance permanently — a subscriber with 1 LXC left could never spend it,
//     because every real request costs more than that.
//   - ALLOWING the overflow is OVERAGE, which the item forbids in capitals, and it
//     is the thing that turns a bounded worst case into an unbounded one.
//
// Clamping is the only behaviour where the allowance is fully usable AND the ceiling
// holds. `covered` is never more than what remained; the DB CHECK is the backstop.
//
// ⚠ THE UPDATE IS THE READ. There is no SELECT-then-UPDATE: `LEAST(remaining, cost)`
// is computed inside the statement, so two concurrent settles serialise on the row
// and the second sees the first's consumption. A check-then-write has a window that
// two settles fit inside, and the DB CHECK would then reject the loser outright —
// turning a benign race into a failed settle.
func (s *Service) Consume(ctx context.Context, workspaceID string, costULXC int64, at time.Time) (covered int64, err error) {
	if costULXC <= 0 {
		return 0, nil
	}
	err = s.pool.QueryRow(ctx, `
		WITH target AS (
			SELECT id, granted_ulxc - consumed_ulxc AS remaining
			FROM subscription_allowance
			WHERE workspace_id = $1 AND period_start <= $2 AND period_end > $2
			ORDER BY period_start DESC LIMIT 1
			FOR UPDATE
		)
		UPDATE subscription_allowance a
		SET consumed_ulxc = a.consumed_ulxc + LEAST(t.remaining, $3::bigint),
		    updated_at = NOW()
		FROM target t
		WHERE a.id = t.id
		RETURNING LEAST(t.remaining, $3::bigint)`,
		workspaceID, at, costULXC).Scan(&covered)
	if errors.Is(err, pgx.ErrNoRows) {
		// No allowance for this workspace right now: nothing is covered, and the
		// caller charges the whole cost as it did before subscriptions existed.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("billing: consume allowance: %w", err)
	}
	return covered, nil
}
