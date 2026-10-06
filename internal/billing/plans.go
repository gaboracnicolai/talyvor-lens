package billing

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// plans.go — B28.439: what each plan costs and includes, for anyone to read. The Plans page shows a visitor
// the figure a subscriber would be granted, and that figure must come from here — never retyped from the
// formula — so it is the same includedUsage a new subscriber's first grant this month reads.

// PublicPlans are the plans the public read describes, in the order a Plans page shows them. BYOK is not
// one: it is a platform fee and includes no usage.
var PublicPlans = []string{"plus", "pro", "max"}

// Plan is one plan as a visitor sees it: its price and the usage it includes this month.
type Plan struct {
	ID           string `json:"id"`
	USDCents     int64  `json:"usd_cents"`
	IncludedULXC int64  `json:"included_ulxc"`
}

// priceReader reads a Price's amount from Stripe (satisfied by *LiveStripe). Optional, as planChangeAPI
// is: a test double without it still builds a Service, and that Service describes no plan.
type priceReader interface {
	PriceUSDCents(ctx context.Context, priceID string) (int64, error)
}

// Plans returns each of PublicPlans this Service sells: the price Stripe bills it at and its included
// usage for the month of `now` — this month's stored figure, or the one the month's first grant stores.
// A plan with no USD price is left out: its subscriber's grant has no fee to size it from.
func (s *Service) Plans(ctx context.Context, now time.Time) ([]Plan, error) {
	api, ok := s.subStripe.(priceReader)
	if !ok {
		return []Plan{}, nil
	}
	out := []Plan{}
	for _, id := range PublicPlans {
		priceID := s.subPlans[id]
		if priceID == "" {
			continue
		}
		fee, err := s.planFee(ctx, api, priceID)
		if err != nil {
			return nil, fmt.Errorf("billing: read the %s price: %w", id, err)
		}
		if fee <= 0 {
			continue
		}
		d, err := s.includedUsage(ctx, fee, now)
		if err != nil {
			return nil, err
		}
		out = append(out, Plan{ID: id, USDCents: fee, IncludedULXC: d})
	}
	return out, nil
}

// CompanyPlans are the company plans the public read prices beside PublicPlans (B32.77), in the order /pricing
// shows them. They are priced per workspace and include no usage.
var CompanyPlans = []string{TeamPlan, BusinessPlan}

// PlanPrice is a plan's price alone.
type PlanPrice struct {
	ID       string `json:"id"`
	USDCents int64  `json:"usd_cents"`
}

// CompanyPrices returns each of CompanyPlans this Service sells at the price Stripe bills it at, and the BYOK
// add-on's price — 0 when this Service does not sell BYOK. Each is read through planFee exactly as Plans
// reads Plus's, Pro's and Max's, and a plan with no USD price is left out, never priced at zero.
func (s *Service) CompanyPrices(ctx context.Context) ([]PlanPrice, int64, error) {
	api, ok := s.subStripe.(priceReader)
	if !ok {
		return []PlanPrice{}, 0, nil
	}
	read := func(id string) (int64, error) {
		priceID := s.subPlans[id]
		if priceID == "" {
			return 0, nil
		}
		fee, err := s.planFee(ctx, api, priceID)
		if err != nil {
			return 0, fmt.Errorf("billing: read the %s price: %w", id, err)
		}
		return max(fee, 0), nil
	}
	out := []PlanPrice{}
	for _, id := range CompanyPlans {
		fee, err := read(id)
		if err != nil {
			return nil, 0, err
		}
		if fee > 0 {
			out = append(out, PlanPrice{ID: id, USDCents: fee})
		}
	}
	byok, err := read(BYOKPlan)
	if err != nil {
		return nil, 0, err
	}
	return out, byok, nil
}

// EnterpriseFromUSDCentsDefault is what Enterprise costs from, a month: Nicolai's decision of 5 Oct 2026,
// "Enterprise from $2,500 a month" (B32.10). Enterprise is contracted, never a Stripe Price, so /pricing reads
// this figure from LENS_ENTERPRISE_FROM_USD_CENTS.
const EnterpriseFromUSDCentsDefault = 250_000

// EnterpriseFromUSDCents is LENS_ENTERPRISE_FROM_USD_CENTS's value v: whole US cents above zero, or the
// default when unset. Lens will not start with a malformed one.
func EnterpriseFromUSDCents(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return EnterpriseFromUSDCentsDefault, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("billing: LENS_ENTERPRISE_FROM_USD_CENTS=%q must be whole US cents above zero, like %d", v, EnterpriseFromUSDCentsDefault)
	}
	return n, nil
}

// planFee is a Price's USD amount, read from Stripe once per Price id: a Stripe Price's amount cannot be
// changed, so a public read never needs to ask twice.
func (s *Service) planFee(ctx context.Context, api priceReader, priceID string) (int64, error) {
	if v, ok := s.planFees.Load(priceID); ok {
		return v.(int64), nil
	}
	fee, err := api.PriceUSDCents(ctx, priceID)
	if err != nil {
		return 0, err
	}
	s.planFees.Store(priceID, fee)
	return fee, nil
}
