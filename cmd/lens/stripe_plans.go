package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/billing"
)

// planFinder names the plan Prices of a Stripe key's account (satisfied by *billing.LiveStripe).
type planFinder interface {
	PlanPrices(ctx context.Context) (map[string]string, error)
}

// sellablePlans is the named plans a Service with this Stripe key sells (B17.21): the configured ones, or — on
// a Stripe TEST-MODE key with none configured — the plan Prices its account holds under the lookup keys B13.1
// gave them (billing.PlanLookupKeys), so a test user can subscribe on a deployment whose env names no plan. A
// live key sells only what env configures. Every amount stays in Stripe; Lens learns only the Price ids.
func sellablePlans(ctx context.Context, configured map[string]string, key string, s planFinder, env string) map[string]string {
	if len(configured) > 0 || key == "" || billing.LiveKey(key) {
		return configured
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	found, err := s.PlanPrices(ctx)
	if err != nil {
		slog.Warn("billing: "+env+" is not set and Stripe's plan Prices could not be read — no plan is sold", "err", err)
		return configured
	}
	if len(found) == 0 {
		slog.Warn("billing: " + env + " is not set and this Stripe test-mode account has no talyvor_*_monthly Price — no plan is sold")
		return configured
	}
	slog.Info("billing: "+env+" is not set — selling the plans this Stripe test-mode account holds", "plans", found)
	return found
}
