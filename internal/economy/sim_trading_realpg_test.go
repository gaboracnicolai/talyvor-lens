package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// B22.8 — on the migrated schema, with the ECB's reference rates of two days: an agent's simulated portfolio of
// 10 000 USD buys euros and pounds at market, places a limit sale of euros over the quote and a limit purchase
// of yen under it (both stay open), and on the next day's rates both cross and fill at the quote; it then sells
// its pounds. Cash, positions and value are exact to the µUSD. A live order is refused as class RED, and no
// credits or money move: the agent's wallet has no posting and the workspace no lxc_ledger row.
func TestSimTrading_MarketAndLimitOrdersAtECBPrices(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ('ws-t', 'ws-t', 'ws-t')`); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAgent(ctx, "ws-t", "trader", "user-t")
	if err != nil {
		t.Fatal(err)
	}
	day := func(date, usd, gbp, jpy string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO ecb_reference_rates (rate_date, currency, per_eur) VALUES
			($1::date, 'USD', $2::numeric), ($1::date, 'GBP', $3::numeric), ($1::date, 'JPY', $4::numeric)`, date, usd, gbp, jpy); err != nil {
			t.Fatal(err)
		}
	}
	day("2026-09-25", "1.1000", "0.8500", "160.00")
	const usd = int64(1_000_000)
	pf, err := s.OpenPortfolio(ctx, "ws-t", a.ID, "fx", 10_000*usd)
	if err != nil || !pf.Simulated || pf.ValueUUSD != 10_000*usd {
		t.Fatalf("open = %+v, %v", pf, err)
	}
	order := func(in SimOrderInput) SimOrder {
		t.Helper()
		o, err := s.PlaceSimOrder(ctx, "ws-t", a.ID, pf.ID, in)
		if err != nil || !o.Simulated {
			t.Fatalf("order %+v = %+v, %v", in, o, err)
		}
		return o
	}
	if o := order(SimOrderInput{Instrument: "eur", Side: "buy", Type: "market", QuantityMicros: 1000 * usd}); o.Status != "filled" ||
		o.FillPriceUSD != "1.1" || o.CashUUSD != -1100*usd {
		t.Fatalf("market buy of 1000 EUR = %+v; want filled at 1.1 USD for 1100 USD", o)
	}
	order(SimOrderInput{Instrument: "GBP", Side: "buy", Type: "market", QuantityMicros: 500 * usd})
	sellEUR := order(SimOrderInput{Instrument: "EUR", Side: "sell", Type: "limit", QuantityMicros: 400 * usd, LimitPriceUSD: "1.15"})
	buyJPY := order(SimOrderInput{Instrument: "JPY", Side: "buy", Type: "limit", QuantityMicros: 100_000 * usd, LimitPriceUSD: "0.0065"})
	if sellEUR.Status != "open" || buyJPY.Status != "open" {
		t.Fatalf("limit orders the quote does not cross = %s, %s; want both open", sellEUR.Status, buyJPY.Status)
	}
	if _, err := s.PlaceSimOrder(ctx, "ws-t", a.ID, pf.ID, SimOrderInput{Instrument: "GBP", Side: "sell", Type: "market", QuantityMicros: 501 * usd}); !errors.Is(err, ErrSimHoldings) {
		t.Fatalf("selling more pounds than held = %v, want refused", err)
	}
	if _, err := s.PlaceSimOrder(ctx, "ws-t", a.ID, pf.ID, SimOrderInput{Instrument: "EUR", Side: "buy", Type: "market", QuantityMicros: usd, Mode: "live"}); !errors.Is(err, ErrLiveTrading) || !strings.Contains(err.Error(), "class RED") {
		t.Fatalf("a live order = %v, want refused as class RED", err)
	}
	if res, err := s.RunSimulatedOrders(ctx); err != nil || res.Filled != 0 {
		t.Fatalf("a tick on the same day's rates = %+v, %v; want nothing filled", res, err)
	}

	// The next day's rates cross both limits: they fill at the quote, not at the limit.
	day("2026-09-26", "1.1600", "0.8600", "180.00")
	if res, err := s.RunSimulatedOrders(ctx); err != nil || res.Filled != 2 {
		t.Fatalf("a tick on the new day's rates = %+v, %v; want both limit orders filled", res, err)
	}
	if o, _ := s.simOrder(ctx, sellEUR.ID); o.Status != "filled" || o.FillPriceUSD != "1.16" || o.FillRateDate != "2026-09-26" || o.CashUUSD != 464*usd {
		t.Fatalf("the euro sale = %+v; want filled at 1.16 USD on 2026-09-26 for 464 USD", o)
	}
	order(SimOrderInput{Instrument: "GBP", Side: "sell", Type: "market", QuantityMicros: 500 * usd})

	pf, err = s.GetPortfolio(ctx, "ws-t", a.ID, pf.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 10 000 − 1100 − 647.058824 + 464 − 644.444445 + 674.418604 USD of cash; 600 EUR at 1.16 and 100 000 JPY at
	// 1.16 ÷ 180, the pounds gone.
	if pf.CashUUSD != 8_746_915_335 || len(pf.Positions) != 2 || pf.Positions[0].Instrument != "EUR" || pf.Positions[0].ValueUUSD != 696*usd ||
		pf.Positions[1].Instrument != "JPY" || pf.Positions[1].ValueUUSD != 644_444_444 || pf.ValueUUSD != 10_087_359_779 {
		t.Fatalf("portfolio = cash %d, positions %+v, value %d; want 8746915335, EUR 696000000 and JPY 644444444, 10087359779",
			pf.CashUUSD, pf.Positions, pf.ValueUUSD)
	}
	if !strings.Contains(pf.MarketData, "ECB") || pf.RateDate != "2026-09-26" {
		t.Errorf("portfolio names its data %q of %q; want the ECB, 2026-09-26", pf.MarketData, pf.RateDate)
	}
	var postings, ledger int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_postings WHERE workspace_id = 'ws-t'),
		(SELECT count(*) FROM lxc_ledger WHERE workspace_id = 'ws-t')`).Scan(&postings, &ledger); err != nil || postings != 0 || ledger != 0 {
		t.Fatalf("wallet postings %d, lxc_ledger rows %d, %v; want none — the simulator moves no credits", postings, ledger, err)
	}
}
