package market

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	stripe "github.com/stripe/stripe-go/v81"
)

// stripeMeter accepts every use but those it refuses, as Stripe answers a meter event it will never take;
// down answers every use as Stripe does when it is unavailable.
type stripeMeter struct {
	refuse   map[string]bool
	down     bool
	tried    map[string]int
	accepted []string
}

func (m *stripeMeter) MeterMarketUse(_ context.Context, _, useID string, _ int64, _ time.Time) error {
	m.tried[useID]++
	switch {
	case m.down:
		return &stripe.Error{HTTPStatusCode: 503, Msg: "Stripe is temporarily unavailable"}
	case m.refuse[useID]:
		return &stripe.Error{HTTPStatusCode: 400, Code: stripe.ErrorCodeResourceMissing, Msg: "No such customer: 'cus_gone'"}
	}
	m.accepted = append(m.accepted, useID)
	return nil
}

// B26.3 — one use Stripe refuses, ahead of three good ones, no longer keeps them off their bills: the
// three are metered in the same pass, the refused one waits out its backoff, and its fifth refusal parks
// it with Stripe's reason. Stripe being down refuses nothing and parks nothing.
func TestMeterPending_ARefusedUseIsSkippedThenParked(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, buyer, price = "ws-seller", "ws-buyer", int64(500_000)
	for _, ws := range []string{seller, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(pool)
	l, err := s.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", PricePerUseULXC: price, Visibility: "public",
		Artifact: json.RawMessage(`{"template":"What is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`)})
	if err != nil {
		t.Fatal(err)
	}
	// Four uses while Stripe is unreachable: each is answered and left for MeterPending. The first is the one
	// Stripe will refuse.
	outage := &stripeMeter{down: true, tried: map[string]int{}}
	var ids []string
	for range 4 {
		u, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: outage}, buyer, "", l.ID, UseRequest{Variables: map[string]string{"a": "1", "b": "2"}})
		if err != nil || u.Charge != ChargeBilled || u.MeterError == "" {
			t.Fatalf("use = %+v, %v; want it answered, billed and not yet metered", u, err)
		}
		ids = append(ids, u.ID)
	}
	bad, good := ids[0], ids[1:]

	type row struct {
		metered, retryAt, parked *time.Time
		refusals                 int
		reason                   string
	}
	read := func(id string) row {
		t.Helper()
		var r row
		if err := pool.QueryRow(ctx, `SELECT metered_at, meter_retry_at, meter_parked_at, meter_refusals, meter_refused_reason
			FROM market_uses WHERE id = $1`, id).Scan(&r.metered, &r.retryAt, &r.parked, &r.refusals, &r.reason); err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Stripe still down: the pass stops, and nothing is counted against any use.
	if n, err := s.MeterPending(ctx, outage, -time.Minute); err == nil || n != 0 {
		t.Fatalf("MeterPending with Stripe down = %d, %v; want 0 and its error", n, err)
	}
	for _, id := range ids {
		if r := read(id); r.refusals != 0 || r.retryAt != nil || r.parked != nil || r.metered != nil {
			t.Fatalf("use %s after Stripe was down = %+v; want untouched", id, r)
		}
	}

	m := &stripeMeter{refuse: map[string]bool{bad: true}, tried: map[string]int{}}
	n, err := s.MeterPending(ctx, m, -time.Minute)
	if err != nil || n != 3 {
		t.Fatalf("MeterPending = %d, %v; want the 3 good uses billed and no error", n, err)
	}
	for _, id := range good {
		if r := read(id); r.metered == nil {
			t.Errorf("good use %s behind the refused one is not on its bill: %+v", id, r)
		}
	}
	const reason = "resource_missing: No such customer: 'cus_gone'"
	r := read(bad)
	if r.metered != nil || r.refusals != 1 || r.reason != reason || r.parked != nil || r.retryAt == nil || !r.retryAt.After(time.Now()) {
		t.Fatalf("refused use after one pass = %+v; want unmetered, 1 refusal with Stripe's reason, a retry ahead, not parked", r)
	}

	// Inside its backoff it is not tried again.
	if _, err := s.MeterPending(ctx, m, -time.Minute); err != nil || m.tried[bad] != 1 {
		t.Fatalf("a pass inside the backoff tried the refused use %d times (%v); want 1 in all", m.tried[bad], err)
	}
	// Each time its backoff runs out it is tried again, until its fifth refusal parks it.
	for i := 2; i <= MaxMeterRefusals; i++ {
		if _, err := pool.Exec(ctx, `UPDATE market_uses SET meter_retry_at = now() - interval '1 second' WHERE id = $1`, bad); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MeterPending(ctx, m, -time.Minute); err != nil {
			t.Fatal(err)
		}
		if r := read(bad); r.refusals != i || (r.parked != nil) != (i == MaxMeterRefusals) {
			t.Fatalf("after refusal %d the use = %+v; want %d refusals, parked only at %d", i, r, i, MaxMeterRefusals)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE market_uses SET meter_retry_at = NULL WHERE id = $1`, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MeterPending(ctx, m, -time.Minute); err != nil || m.tried[bad] != MaxMeterRefusals {
		t.Fatalf("a parked use was tried %d times (%v); want %d — parked is never retried", m.tried[bad], err, MaxMeterRefusals)
	}
	if len(m.accepted) != 3 {
		t.Errorf("Stripe accepted %v; want each good use once", m.accepted)
	}

	parked, err := s.ParkedUses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(parked) != 1 || parked[0].ID != bad || parked[0].Reason != reason || parked[0].Refusals != MaxMeterRefusals ||
		parked[0].BuyerWorkspaceID != buyer || parked[0].ListingID != l.ID || parked[0].PriceULXC != price {
		t.Fatalf("parked uses = %+v; want only %s, refused %d times: %q", parked, bad, MaxMeterRefusals, reason)
	}

	// B27.19 — an operator retries it. Stripe refusing it once more parks it again at once; retried again
	// after the operator fixed the buyer's customer, the next pass puts it on the buyer's bill.
	if err := s.RetryParkedUse(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if r := read(bad); r.parked != nil || r.retryAt != nil {
		t.Fatalf("retried use = %+v; want un-parked and due now", r)
	}
	if _, err := s.MeterPending(ctx, m, -time.Minute); err != nil || m.tried[bad] != MaxMeterRefusals+1 {
		t.Fatalf("the pass after a retry tried the use %d times (%v); want once more", m.tried[bad], err)
	}
	if r := read(bad); r.parked == nil || r.refusals != MaxMeterRefusals+1 || r.metered != nil {
		t.Fatalf("a retried use Stripe refused again = %+v; want parked again with %d refusals", r, MaxMeterRefusals+1)
	}
	if err := s.RetryParkedUse(ctx, bad); err != nil {
		t.Fatal(err)
	}
	delete(m.refuse, bad)
	if n, err := s.MeterPending(ctx, m, -time.Minute); err != nil || n != 1 {
		t.Fatalf("MeterPending after the retry = %d, %v; want the retried use billed", n, err)
	}
	if r := read(bad); r.metered == nil || r.parked != nil {
		t.Fatalf("retried use after Stripe accepted it = %+v; want metered", r)
	}
	if parked, err := s.ParkedUses(ctx); err != nil || len(parked) != 0 {
		t.Fatalf("parked uses after the retry billed = %+v, %v; want none", parked, err)
	}
	if err := s.RetryParkedUse(ctx, bad); !errors.Is(err, ErrNotParked) {
		t.Fatalf("retrying a metered use = %v; want ErrNotParked", err)
	}
}
