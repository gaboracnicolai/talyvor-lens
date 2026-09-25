package billing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/alerts"
)

// included_usage.go — B13.1: a plan's included usage D is COMPUTED from its price, not chosen, so a
// subscriber who uses all of it can never cost Talyvor money:
//
//	D = F × (1 − s) × (1 − 0.3·h) / (1 − h)
//
// F = the plan's price · s = the card fee on that price · h = the share of subscriber chat traffic, by
// metered value, served from the pool over the last 30 days. Talyvor charges provider cost with no
// markup, and a pooled answer costs Talyvor ~nothing upstream while drawing 0.7 × list from the
// allowance: a subscriber drawing D spends T = D / (1 − 0.3h) of metered value, of which (1 − h)·T
// is paid upstream — exactly F × (1 − s) at this D.
//
// GUARDRAILS (Nicolai): h only from production token_events; the conservative (Wilson 95%) lower bound,
// not the point estimate; h = 0 until 1,000 subscriber requests in the window; D rises at most 25% a
// month; recomputed monthly — the first grant of a month computes that month's figure from the 30 days
// before the 1st, and every later grant that month reuses it. Past D a subscriber continues on prepaid
// credit (B1.6), never overage on the plan.

const (
	// pooledDiscount is the consumer discount on a pooled answer (LENS_POOL_CONSUMER_DISCOUNT's default).
	pooledDiscount = 0.3
	// minWindowRequests is the floor below which h is not measured: it stays 0.
	minWindowRequests = 1000
	// maxMonthlyRise caps D at 125% of the previous month's figure for the same price.
	maxMonthlyRise = 1.25
	// maxPooledShare keeps (1 − h) away from zero; the monthly cap binds long before it matters.
	maxPooledShare = 0.9
)

// IncludedUsageULXC is D, in µLXC (rounded DOWN — an allowance never exceeds the formula), for a plan
// priced feeCents at pooled share h. s is Stripe's standard card pricing, 2.9% + 30¢, so
// F × (1 − s) = 0.971·F − 30¢, computed in exact integers.
func IncludedUsageULXC(feeCents int64, h float64) int64 {
	if feeCents <= 0 {
		return 0
	}
	h = math.Max(0, math.Min(h, maxPooledShare))
	// F × (1 − s) in exact integers: F·(1 − 0.029) − 30¢ is (971·F − 30,000) thousandths of a cent.
	base := (971*feeCents - 30_000) * ulxcPerCent / 1000
	if base <= 0 {
		return 0
	}
	if h == 0 {
		return base
	}
	return int64(math.Floor(float64(base) * (1 - pooledDiscount*h) / (1 - h)))
}

// wilsonLower is the Wilson score interval's lower bound for a share p observed over n requests (z=1.96).
func wilsonLower(p float64, n int64) float64 {
	if n <= 0 {
		return 0
	}
	const z = 1.96
	nf := float64(n)
	lb := (p + z*z/(2*nf) - z*math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))) / (1 + z*z/nf)
	return math.Max(0, lb)
}

// monthOf is the 1st of t's month, UTC.
func monthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// pooledShareSQL reads subscriber chat traffic in the window: session-key requests from a workspace inside
// an allowance period at the time, served upstream or from the pool. Own-cache hits are neither.
const pooledShareSQL = `
SELECT t.model, t.input_tokens, t.output_tokens, t.cost_usd, t.serve_source
FROM token_events t
WHERE t.auth_method = 'session_key'
  AND t.created_at >= $1 AND t.created_at < $2
  AND COALESCE(t.serve_source, 'upstream') IN ('upstream', 'cache_hit_pooled', 'cache_hit_pooled_semantic')
  AND EXISTS (SELECT 1 FROM subscription_allowance a
              WHERE a.workspace_id = t.workspace_id AND t.created_at >= a.period_start AND t.created_at < a.period_end)`

// MeasurePooledShare returns h for the month starting `month` — the conservative lower bound of the
// pooled share, by metered value, of subscriber chat traffic in the 30 days before it — and the number
// of requests it was measured over. h is 0 below the request floor.
func (s *Service) MeasurePooledShare(ctx context.Context, month time.Time) (h float64, requests int64, err error) {
	rows, err := s.pool.Query(ctx, pooledShareSQL, month.AddDate(0, 0, -30), month)
	if err != nil {
		return 0, 0, fmt.Errorf("billing: measure pooled share: %w", err)
	}
	defer rows.Close()
	var pooled, total float64
	for rows.Next() {
		var model, source string
		var in, out int
		var cost float64
		if err := rows.Scan(&model, &in, &out, &cost, &source); err != nil {
			return 0, 0, err
		}
		requests++
		if source == "upstream" {
			total += cost
			continue
		}
		// A pooled serve's metered value is what the live call would have cost (its list price).
		list := alerts.CostUSD(model, in, out)
		pooled += list
		total += list
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if requests < minWindowRequests || total <= 0 {
		return 0, requests, nil
	}
	return wilsonLower(pooled/total, requests), requests, nil
}

// includedUsage is D for a period of a plan priced feeCents starting at periodStart: this month's stored
// figure for that price, or — at the month's first grant — computed from measured h, capped at 125% of
// the previous month's figure (of D at h = 0 when there is none), and stored.
func (s *Service) includedUsage(ctx context.Context, feeCents int64, periodStart time.Time) (int64, error) {
	month := monthOf(periodStart)
	var d int64
	err := s.pool.QueryRow(ctx, `SELECT included_ulxc FROM plan_included_usage WHERE month = $1 AND fee_usd_cents = $2`,
		month, feeCents).Scan(&d)
	if err == nil {
		return d, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("billing: read included usage: %w", err)
	}
	h, n, err := s.MeasurePooledShare(ctx, month)
	if err != nil {
		return 0, err
	}
	base := IncludedUsageULXC(feeCents, 0)
	var prev int64
	if err := s.pool.QueryRow(ctx, `SELECT included_ulxc FROM plan_included_usage WHERE month = $1 AND fee_usd_cents = $2`,
		month.AddDate(0, -1, 0), feeCents).Scan(&prev); err == nil && prev > 0 {
		base = prev
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("billing: read last month's included usage: %w", err)
	}
	d = IncludedUsageULXC(feeCents, h)
	if ceiling := int64(math.Floor(float64(base) * maxMonthlyRise)); d > ceiling {
		d = ceiling
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO plan_included_usage (month, fee_usd_cents, pooled_share, window_requests, included_ulxc)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (month, fee_usd_cents) DO NOTHING`,
		month, feeCents, h, n, d); err != nil {
		return 0, fmt.Errorf("billing: store included usage: %w", err)
	}
	// A concurrent first grant may have stored the month's figure first; that one is the month's.
	if err := s.pool.QueryRow(ctx, `SELECT included_ulxc FROM plan_included_usage WHERE month = $1 AND fee_usd_cents = $2`,
		month, feeCents).Scan(&d); err != nil {
		return 0, fmt.Errorf("billing: re-read included usage: %w", err)
	}
	return d, nil
}
