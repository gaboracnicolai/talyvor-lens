package market

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/economy"
)

// askedCapabilities answers every capability, and remembers which were asked.
type askedCapabilities []string

func (a *askedCapabilities) RequireBilledCapability(_ context.Context, _, key string) error {
	*a = append(*a, key)
	return nil
}

// promptsSeen answers "ok" and keeps every prompt it was sent.
type promptsSeen []string

func (p *promptsSeen) Run(_ context.Context, _ string, messages []Message) (string, error) {
	*p = append(*p, messages[len(messages)-1].Content)
	return "ok", nil
}

// B32.19 — renting for 30 days writes one billed rent row and an active licence (the same key again buys nothing);
// the next five uses are 'licensed' rows that never reach the meter; 31 days on the licence reads expired and the
// next use bills the per_use price. Past a subscription's included uses the per_use offer bills. An agent key is
// billed per use beside its workspace's personal licence; an agent whose licence pins version 1 runs version 1
// after version 2 publishes. Once the invoice is paid, the rent row clears 85/15 on the journal.
func TestLicences_RentCoversUsesUntilItEndsAndPinsItsVersion(t *testing.T) {
	ctx := context.Background()
	pool := migratedDB(t)
	const seller, renter, subscriber, team = "ws-b3219-seller", "ws-b3219-renter", "ws-b3219-sub", "ws-b3219-team"
	for _, ws := range []string{seller, renter, subscriber, team} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(pool)
	days := func(n int) *int { return &n }
	l, err := s.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", Visibility: "public",
		Artifact: json.RawMessage(`{"template":"v1: what is {{a}} + {{b}}?","model":"claude-haiku-4-5"}`),
		Offers: []Offer{
			{Kind: OfferPerUse, Licence: LicenceCommercial, PriceUSDMicros: 50_000},
			{Kind: OfferRent, Licence: LicenceCommercial, PriceUSDMicros: 1_000_000, PeriodDays: days(30)},
			{Kind: OfferSubscribe, Licence: LicenceCommercial, PriceUSDMicros: 400_000, PeriodDays: days(30), IncludedUses: days(1)},
			{Kind: OfferBuy, Licence: LicencePersonal, PriceUSDMicros: 2_000_000},
			{Kind: OfferBuy, Licence: LicenceCommercial, PriceUSDMicros: 9_000_000},
		}})
	if err != nil {
		t.Fatal(err)
	}
	offer := map[string]string{}
	for _, o := range l.Offers {
		offer[o.Kind+" "+o.Licence] = o.ID
	}
	meter := &stripeMeter{tried: map[string]int{}}
	var asked askedCapabilities
	deps := LicenceDeps{Meter: meter, Capabilities: &asked}
	add := UseRequest{Variables: map[string]string{"a": "1", "b": "2"}, Person: "user:ana"}
	row := func(useID string) (charge, kind string, price int64, licence string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT charge, use_kind, price_ulxc, COALESCE(licence_id, '') FROM market_uses WHERE id = $1`, useID).
			Scan(&charge, &kind, &price, &licence); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Renting for 30 days: one billed rent row at the offer's price, metered, and an active licence.
	rent, again, err := s.License(ctx, deps, renter, "", l.ID, "rent-1", LicenceRequest{OfferID: offer["rent commercial"], Person: "user:ana"})
	if err != nil || again {
		t.Fatalf("rent = %+v, again %v, %v", rent, again, err)
	}
	if c, k, p, lic := row(rent.UseID); c != ChargeBilled || k != OfferRent || p != 10_000_000 || lic != rent.ID {
		t.Fatalf("the rent's row = %s %s at %d µLXC for %q; want one billed rent at 10,000,000 µLXC for %s", c, k, p, lic, rent.ID)
	}
	if rent.Status != LicenceActive || rent.EndsAt == nil || rent.EndsAt.Sub(rent.StartsAt) != 30*24*time.Hour || rent.RentPaidUSDMicros != 0 {
		t.Fatalf("the licence = %+v; want active for 30 days, no rent paid until it clears (B32.20)", rent)
	}
	if !slices.Equal(meter.accepted, []string{rent.UseID}) || !slices.Equal(asked, []string{economy.CapabilityRentAndSubscribe}) {
		t.Fatalf("metered %v, asked %v; want the rent's row metered once and rent_and_subscribe_listings asked", meter.accepted, asked)
	}
	if same, again, err := s.License(ctx, deps, renter, "", l.ID, "rent-1", LicenceRequest{OfferID: offer["rent commercial"]}); err != nil || !again || same.ID != rent.ID {
		t.Fatalf("the same key again = %+v, again %v, %v; want the same licence", same, again, err)
	}

	// The next five uses: 'licensed' rows at 0, and no meter event.
	for range 5 {
		u, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: meter}, renter, "", l.ID, add)
		if err != nil {
			t.Fatal(err)
		}
		if c, k, p, lic := row(u.ID); c != ChargeLicensed || k != "use" || p != 0 || lic != rent.ID {
			t.Fatalf("a use under the rent = %s %s at %d for %q; want licensed at 0 under %s", c, k, p, lic, rent.ID)
		}
	}
	if len(meter.accepted) != 1 {
		t.Fatalf("metered %v; want only the rent", meter.accepted)
	}

	// 31 days on: the licence reads expired, and the next use bills the per_use price.
	s.clock = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	list, err := s.Licences(ctx, renter)
	if err != nil || len(list) != 1 || list[0].Status != LicenceExpired || list[0].UsesCovered != 5 || list[0].UseID != rent.UseID {
		t.Fatalf("31 days on, the renter's licences = %+v, %v; want the rent expired, having covered 5 uses", list, err)
	}
	late, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: meter}, renter, "", l.ID, add)
	if err != nil {
		t.Fatal(err)
	}
	if c, _, p, lic := row(late.ID); c != ChargeBilled || p != 500_000 || lic != "" {
		t.Fatalf("a use after the rent ended = %s at %d under %q; want billed at the per_use 500,000 µLXC", c, p, lic)
	}
	s.clock = nil

	// A subscription with one included use: the second use is billed per use.
	sub, _, err := s.License(ctx, deps, subscriber, "", l.ID, "sub-1", LicenceRequest{OfferID: offer["subscribe commercial"]})
	if err != nil || !sub.AutoRenew {
		t.Fatalf("subscribe = %+v, %v; want a licence that renews", sub, err)
	}
	var charges []string
	for range 2 {
		u, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: meter}, subscriber, "", l.ID, add)
		if err != nil {
			t.Fatal(err)
		}
		charges = append(charges, u.Charge)
	}
	if !slices.Equal(charges, []string{ChargeLicensed, ChargeBilled}) {
		t.Fatalf("two uses of a subscription including one = %v; want licensed, then billed", charges)
	}

	// A person's personal licence covers them, and never an agent's key.
	if _, _, err := s.License(ctx, deps, team, "agt_b3219", l.ID, "own-agent", LicenceRequest{OfferID: offer["buy personal"]}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an agent key buying a personal licence = %v; want it refused", err)
	}
	if _, _, err := s.License(ctx, deps, team, "", l.ID, "own-ana", LicenceRequest{OfferID: offer["buy personal"], Person: "user:ana"}); err != nil {
		t.Fatal(err)
	}
	byAgent, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: meter}, team, "agt_b3219", l.ID, UseRequest{Variables: add.Variables})
	if err != nil || byAgent.Charge != ChargeBilled || byAgent.PriceULXC != 500_000 {
		t.Fatalf("an agent key beside a personal licence = %+v, %v; want billed per use", byAgent, err)
	}
	if byAna, err := s.Use(ctx, UseDeps{Runner: answers("3"), Meter: meter}, team, "", l.ID, add); err != nil || byAna.Charge != ChargeLicensed {
		t.Fatalf("the person the licence is for = %+v, %v; want licensed", byAna, err)
	}

	// An agent whose licence pins version 1 still runs version 1 after version 2 publishes; naming 2 bills it.
	pinned, _, err := s.License(ctx, deps, team, "agt_b3219", l.ID, "pin-1", LicenceRequest{OfferID: offer["buy commercial"], Version: 1})
	if err != nil || pinned.PinnedVersion == nil || *pinned.PinnedVersion != 1 || pinned.EndsAt != nil {
		t.Fatalf("buy pinned to version 1 = %+v, %v", pinned, err)
	}
	if _, err := s.PublishVersion(ctx, seller, l.ID, json.RawMessage(`{"template":"v2: what is {{a}} plus {{b}}?","model":"claude-haiku-4-5"}`), "v2"); err != nil {
		t.Fatal(err)
	}
	var seen promptsSeen
	u, err := s.Use(ctx, UseDeps{Runner: &seen, Meter: meter}, team, "agt_b3219", l.ID, UseRequest{Variables: add.Variables})
	if err != nil || u.Version != 1 || u.Charge != ChargeLicensed || u.LicenceID != pinned.ID || !strings.HasPrefix(seen[0], "v1:") {
		t.Fatalf("the pinned agent's use after version 2 = %+v (sent %q), %v; want version 1, licensed", u, seen, err)
	}
	if v2, err := s.Use(ctx, UseDeps{Runner: &seen, Meter: meter}, team, "agt_b3219", l.ID, UseRequest{Version: 2, Variables: add.Variables}); err != nil ||
		v2.Version != 2 || v2.Charge != ChargeBilled {
		t.Fatalf("the pinned agent naming version 2 = %+v, %v; want version 2, billed per use", v2, err)
	}

	// The renter's invoice is paid: the rent row clears 85/15 on the journal.
	now := time.Now()
	if _, err := s.ClearInvoice(ctx, renter, "in_b3219", now.Add(-time.Hour), now.Add(time.Hour), now, false); err != nil {
		t.Fatal(err)
	}
	j, err := s.JournalFor(ctx, rent.UseID)
	if err != nil || len(j) != 1 || j[0].Kind != JournalClear {
		t.Fatalf("the rent's journal = %+v, %v; want one clear entry", j, err)
	}
	got := map[string]int64{}
	for _, p := range j[0].Postings {
		got[p.Account] = p.AmountUSDMicros
	}
	if got[AccountStripeClearing] != 1_000_000 || got[AccountMarketFee] != -150_000 || got[SellerHoldback(seller)] != -850_000 {
		t.Fatalf("the rent's clear postings = %v; want +1,000,000 clearing, −150,000 Talyvor's fee, −850,000 to the seller", got)
	}
}
