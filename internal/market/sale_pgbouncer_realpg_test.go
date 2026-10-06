package market

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type answers string

func (a answers) Run(context.Context, string, []Message) (string, error) { return string(a), nil }

type meterCalls []string

func (m *meterCalls) MeterMarketUse(_ context.Context, _, useID string, _ int64, _ time.Time) error {
	*m = append(*m, useID)
	return nil
}

// B17.16 — the testers' marketplace-sale, on the connection production has: behind PgBouncer Lens speaks
// pgx's simple protocol (LENS_DB_PGBOUNCER), where a []byte argument goes out as bytea and a jsonb column
// refuses it (22P02) — every publish was a 500. A seller publishes, a buyer uses the listing and is billed
// once, and the seller's pending earnings rise by exactly their share.
func TestMarketplaceSale_OverPgBouncerSimpleProtocol(t *testing.T) {
	cfg := migratedDB(t).Config()
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const seller, buyer, price = "ws-seller", "ws-buyer", int64(500_000)
	for _, ws := range []string{seller, buyer} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, synthetic, company) VALUES ($1, $1, $1, true, true)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(pool)

	l, err := s.Publish(ctx, seller, Draft{Kind: "prompt", Title: "Adder", PricePerUseULXC: price, Visibility: "public",
		Artifact: json.RawMessage(`{"template":"What is {{a}} + {{b}}? Reply with the number only.","model":"claude-haiku-4-5"}`)})
	if err != nil {
		t.Fatalf("publish over simple protocol: %v", err)
	}
	if _, err := s.PublishVersion(ctx, seller, l.ID, json.RawMessage(`{"template":"What is {{a}} plus {{b}}?","model":"claude-haiku-4-5"}`), "reworded", nil); err != nil {
		t.Fatalf("a new version over simple protocol: %v", err)
	}
	now := time.Now()
	earned0, err := s.SellerEarnings(ctx, seller, now)
	if err != nil {
		t.Fatal(err)
	}

	var metered meterCalls
	u, err := s.Use(ctx, UseDeps{Runner: answers("579"), Meter: &metered}, buyer, "", l.ID,
		UseRequest{Version: 1, Variables: map[string]string{"a": "123", "b": "456"}})
	if err != nil || u.Output != "579" || u.Charge != ChargeBilled {
		t.Fatalf("the buyer's use = %+v, %v; want it answered and billed", u, err)
	}
	if len(metered) != 1 || metered[0] != u.ID {
		t.Errorf("one use metered %v onto the buyer's bill; want [%s]", metered, u.ID)
	}
	bill, err := s.MonthBill(ctx, buyer, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(bill.Lines) != 1 || bill.Lines[0].ListingID != l.ID || bill.Lines[0].PriceULXC != price {
		t.Errorf("the buyer's bill = %+v; want one %d µLXC line for %s", bill.Lines, price, l.ID)
	}
	earned, err := s.SellerEarnings(ctx, seller, now)
	if err != nil {
		t.Fatal(err)
	}
	if share := SellerShare(price/ulxcPerUSDMicro, 1500); earned.PendingUses != earned0.PendingUses+1 || earned.PendingUSDMicros != earned0.PendingUSDMicros+share {
		t.Errorf("the seller's pending went %d → %d uses, %d → %d µUSD; want +1 and +%d", earned0.PendingUses, earned.PendingUses,
			earned0.PendingUSDMicros, earned.PendingUSDMicros, share)
	}
}
