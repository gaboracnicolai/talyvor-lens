package market

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
)

const day = 24 * time.Hour

// renewalsMarket is a seller's listing sold per use, by a 30-day subscription including one use, by a 30-day rent and
// outright at three rents — all commercial — with the economy's tick renewing the marketplace's licences.
func renewalsMarket(t *testing.T, buyer string) (*pgxpool.Pool, *Store, *economy.DualTokenStore, string, map[string]string) {
	t.Helper()
	ctx := context.Background()
	pool := migratedDB(t)
	const seller = "ws-b3220-seller"
	for _, ws := range []string{seller, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(pool)
	bank := economy.NewDualTokenStore(nil, pool, nil)
	bank.SetListingCharger(s)
	n := func(n int) *int { return &n }
	l, err := s.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", Visibility: "public",
		Artifact: json.RawMessage(`{"template":"what is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`),
		Offers: []Offer{
			{Kind: OfferPerUse, Licence: LicenceCommercial, PriceUSDMicros: 50_000},
			{Kind: OfferSubscribe, Licence: LicenceCommercial, PriceUSDMicros: 400_000, PeriodDays: n(30), IncludedUses: n(1)},
			{Kind: OfferRent, Licence: LicenceCommercial, PriceUSDMicros: 1_000_000, PeriodDays: n(30)},
			{Kind: OfferBuy, Licence: LicenceCommercial, PriceUSDMicros: 3_000_000},
		}})
	if err != nil {
		t.Fatal(err)
	}
	offer := map[string]string{}
	for _, o := range l.Offers {
		offer[o.Kind] = o.ID
	}
	return pool, s, bank, l.ID, offer
}

// licenceRows is a licence's purchase and renewals: kind, charge, price in µLXC and when, in order.
type licenceRow struct {
	kind, charge string
	ulxc         int64
	at           time.Time
}

func licenceRows(t *testing.T, pool *pgxpool.Pool, licenceID string) []licenceRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT use_kind, charge, price_ulxc, used_at FROM market_uses
		WHERE licence_id = $1 AND use_kind <> 'use' ORDER BY used_at`, licenceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []licenceRow
	for rows.Next() {
		var r licenceRow
		if err := rows.Scan(&r.kind, &r.charge, &r.ulxc, &r.at); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// B32.20 — a monthly subscription renews on the schedules' tick: three renewals write three billed rows one period
// apart (a tick run twice renews once), each on the buyer's bill, and each period has its own included use. A cancel
// stops the fourth: the licence runs to its end and expires.
func TestRenewals_ASubscriptionRenewsEachPeriodUntilCancelled(t *testing.T) {
	ctx := context.Background()
	const buyer = "ws-b3220-sub"
	pool, s, bank, listing, offer := renewalsMarket(t, buyer)
	meter := &stripeMeter{tried: map[string]int{}}
	base := time.Now().UTC().Add(-130 * day).Truncate(time.Second)
	at := func(d time.Duration) { s.clock = func() time.Time { return base.Add(d) } }
	at(0)
	sub, _, err := s.License(ctx, LicenceDeps{Meter: meter, Capabilities: &askedCapabilities{}}, buyer, "", listing, "sub", LicenceRequest{OfferID: offer[OfferSubscribe]})
	if err != nil || !sub.AutoRenew {
		t.Fatalf("subscribe = %+v, %v; want a licence that renews", sub, err)
	}
	use := func() Use {
		t.Helper()
		u, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: meter}, buyer, "", listing, UseRequest{Variables: map[string]string{"a": "1", "b": "2"}})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	at(day)
	if u := use(); u.Charge != ChargeLicensed {
		t.Fatalf("the first period's use = %s; want licensed", u.Charge)
	}

	for i := 1; i <= 3; i++ {
		for range 2 {
			if _, err := bank.RunAgentSchedules(ctx, base.Add(time.Duration(i)*30*day+time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := licenceRows(t, pool, sub.ID)
	if len(got) != 4 || got[0].kind != OfferSubscribe {
		t.Fatalf("the subscription's rows = %+v; want the subscription and three renewals", got)
	}
	for i, r := range got[1:] {
		if r.kind != "renewal" || r.charge != ChargeBilled || r.ulxc != 4_000_000 || !r.at.Equal(base.Add(time.Duration(i+1)*30*day)) {
			t.Fatalf("renewal %d = %+v; want billed at 4,000,000 µLXC at day %d", i+1, r, (i+1)*30)
		}
	}
	at(91 * day)
	if u := use(); u.Charge != ChargeLicensed {
		t.Fatalf("the fourth period's first use = %s; want licensed: each period includes its own use", u.Charge)
	}
	if n, err := s.MeterPending(ctx, meter, time.Minute); err != nil || n != 3 || len(meter.accepted) != 4 {
		t.Fatalf("metered %d (%v), the bill took %v; want the subscription and its three renewals", n, err, meter.accepted)
	}

	at(100 * day)
	c, err := s.CancelLicence(ctx, buyer, sub.ID)
	if err != nil || c.AutoRenew || c.Status != LicenceActive || !c.EndsAt.Equal(base.Add(120*day)) {
		t.Fatalf("cancel = %+v, %v; want it running to day 120 and renewing no more", c, err)
	}
	if res, err := bank.RunAgentSchedules(ctx, base.Add(121*day)); err != nil || res.Renewed != 0 {
		t.Fatalf("the tick after the cancel = %+v, %v; want nothing renewed", res, err)
	}
	if got := licenceRows(t, pool, sub.ID); len(got) != 4 {
		t.Fatalf("after the cancel, the subscription's rows = %+v; want no fourth renewal", got)
	}
	at(121 * day)
	if list, err := s.Licences(ctx, buyer); err != nil || len(list) != 1 || list[0].Status != LicenceExpired {
		t.Fatalf("after its end, the buyer's licences = %+v, %v; want it expired", list, err)
	}
}

// B32.20 — with the buy price at three rents, a rent that renews bills three rents; clearing the first two owns
// nothing yet, clearing the third issues a perpetual licence at no charge and stops the renewals: no fourth rent is
// billed, and renting again is refused because the buyer owns it.
func TestRenewals_RentsAddUpToOwnership(t *testing.T) {
	ctx := context.Background()
	const buyer = "ws-b3220-renter"
	pool, s, bank, listing, offer := renewalsMarket(t, buyer)
	meter := &stripeMeter{tried: map[string]int{}}
	deps := LicenceDeps{Meter: meter, Capabilities: &askedCapabilities{}}
	base := time.Now().UTC().Add(-100 * day).Truncate(time.Second)
	at := func(d time.Duration) { s.clock = func() time.Time { return base.Add(d) } }
	at(0)
	yes := true
	rent, _, err := s.License(ctx, deps, buyer, "", listing, "rent", LicenceRequest{OfferID: offer[OfferRent], AutoRenew: &yes})
	if err != nil || !rent.AutoRenew || rent.RentPaidUSDMicros != 0 {
		t.Fatalf("rent = %+v, %v; want a rent that renews, nothing paid yet", rent, err)
	}
	for _, d := range []time.Duration{30*day + time.Minute, 60*day + time.Minute} {
		if _, err := bank.RunAgentSchedules(ctx, base.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	if got := licenceRows(t, pool, rent.ID); len(got) != 3 || got[1].kind != "renewal" || got[2].ulxc != 10_000_000 {
		t.Fatalf("the rent's rows = %+v; want the rent and two renewals at 10,000,000 µLXC", got)
	}
	if _, err := s.MeterPending(ctx, meter, time.Minute); err != nil {
		t.Fatal(err)
	}
	owned := func() []Licence {
		t.Helper()
		list, err := s.Licences(ctx, buyer)
		if err != nil {
			t.Fatal(err)
		}
		var out []Licence
		for _, l := range list {
			if l.Source == SourceRentToOwn {
				out = append(out, l)
			}
		}
		return out
	}

	if n, err := s.ClearInvoice(ctx, buyer, "in_b3220_1", base.Add(-time.Hour), base.Add(31*day), base.Add(40*day), false); err != nil || n != 2 {
		t.Fatalf("clearing the first two rents = %d, %v", n, err)
	}
	if o := owned(); len(o) != 0 {
		t.Fatalf("after two rents of three, owned %+v; want nothing yet", o)
	}
	if n, err := s.ClearInvoice(ctx, buyer, "in_b3220_2", base.Add(31*day), base.Add(61*day), base.Add(70*day), false); err != nil || n != 1 {
		t.Fatalf("clearing the third rent = %d, %v", n, err)
	}
	o := owned()
	if len(o) != 1 || o[0].Kind != OfferBuy || o[0].OfferID != offer[OfferBuy] || o[0].EndsAt != nil || o[0].Status != LicenceActive || o[0].UseID != "" {
		t.Fatalf("after the third rent cleared, owned %+v; want one perpetual licence under the buy offer, charged nothing", o)
	}
	var paid int64
	var renews bool
	if err := pool.QueryRow(ctx, `SELECT rent_paid_usd_micros, auto_renew FROM market_licences WHERE id = $1`, rent.ID).Scan(&paid, &renews); err != nil ||
		paid != 3_000_000 || renews {
		t.Fatalf("the rent: paid %d, renews %v (%v); want 3,000,000 µUSD paid and its renewals stopped", paid, renews, err)
	}

	if res, err := bank.RunAgentSchedules(ctx, base.Add(90*day+time.Minute)); err != nil || res.Renewed != 0 {
		t.Fatalf("the tick at the rent's end = %+v, %v; want nothing renewed", res, err)
	}
	if got := licenceRows(t, pool, rent.ID); len(got) != 3 {
		t.Fatalf("the rent's rows = %+v; want no fourth rent", got)
	}
	at(91 * day)
	if _, _, err := s.License(ctx, deps, buyer, "", listing, "rent-4", LicenceRequest{OfferID: offer[OfferRent]}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("renting what the buyer owns = %v; want it refused", err)
	}
	u, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: meter}, buyer, "", listing, UseRequest{Variables: map[string]string{"a": "1", "b": "2"}})
	if err != nil || u.Charge != ChargeLicensed || u.LicenceID != o[0].ID {
		t.Fatalf("a use after the rent ended = %+v, %v; want licensed under the owned licence", u, err)
	}
}

// B32.20 — an agent whose monthly limit its subscription spent cannot renew it: at the period's end the tick writes no
// row, and the licence ends unpaid at its ends_at and renews no more.
func TestRenewals_AnAgentWhoseMonthlyLimitIsSpentEndsItsSubscriptionUnpaid(t *testing.T) {
	ctx := context.Background()
	const buyer = "ws-b3220-agents"
	pool, s, bank, listing, offer := renewalsMarket(t, buyer)
	agent, err := bank.CreateAgent(ctx, buyer, "researcher", "owner-"+buyer)
	if err != nil {
		t.Fatal(err)
	}
	maySubscribe := true // B32.22: an agent subscribes only when its rules let it
	if _, err := bank.SetAgentRules(ctx, buyer, agent.ID, economy.AgentRules{MonthlyLimitULXC: 4_000_000, MaySubscribe: &maySubscribe}); err != nil {
		t.Fatal(err)
	}
	meter := &stripeMeter{tried: map[string]int{}}
	sub, _, err := s.License(ctx, LicenceDeps{Meter: meter, Agents: bank, Capabilities: bank}, buyer, agent.ID, listing, "agent-sub",
		LicenceRequest{OfferID: offer[OfferSubscribe]})
	if err != nil || sub.Charge != ChargeBilled {
		t.Fatalf("the agent subscribing = %+v, %v; want it billed within its monthly limit", sub, err)
	}
	for i, ended := range []int{1, 0} {
		res, err := bank.RunAgentSchedules(ctx, sub.EndsAt.Add(time.Minute))
		if err != nil || res.Renewed != 0 || res.Unpaid != ended {
			t.Fatalf("tick %d at its end = %+v, %v; want nothing renewed and %d ended unpaid", i+1, res, err, ended)
		}
	}
	if got := licenceRows(t, pool, sub.ID); len(got) != 1 {
		t.Fatalf("the subscription's rows = %+v; want only the subscription", got)
	}
	var status string
	var endsAt time.Time
	var renews bool
	if err := pool.QueryRow(ctx, `SELECT status, ends_at, auto_renew FROM market_licences WHERE id = $1`, sub.ID).Scan(&status, &endsAt, &renews); err != nil ||
		status != LicenceUnpaid || !endsAt.Equal(*sub.EndsAt) || renews {
		t.Fatalf("the licence = %s ending %v, renews %v (%v); want unpaid at its ends_at %v, renewing no more", status, endsAt, renews, err, sub.EndsAt)
	}
}
