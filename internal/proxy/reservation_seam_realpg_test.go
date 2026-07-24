package proxy

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/poolroyalty"
)

// Real-PG proofs that the reservation SEAM bills what was DELIVERED. The seam helpers (agentReserveBlocks →
// settleReservation / resolveCacheReservation / releaseReservation) run against a real economy.DualTokenStore
// so the assertions are on the workspace BALANCE and lxc_ledger — never a status code.

func seamProxy(t *testing.T) (*Proxy, *economy.DualTokenStore, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG reservation-seam test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS lxc_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			lifetime_minted BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS lxc_ledger (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS agent_lxc_subbudgets (scoped_key_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL,
			ceiling_lxc BIGINT NOT NULL DEFAULT 50000000, spent_lxc BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS lxc_reservations (reservation_id TEXT PRIMARY KEY, scoped_key_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL, held_ulxc BIGINT NOT NULL, settled_ulxc BIGINT,
			status TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held','settled','released')),
			requested_model TEXT, request_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), resolved_at TIMESTAMPTZ)`,
		`TRUNCATE lxc_balances, lxc_ledger, agent_lxc_subbudgets, lxc_reservations`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	store := economy.NewDualTokenStore(nil, pool, nil)
	p := &Proxy{}
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	return p, store, pool
}

func seamFund(t *testing.T, pool *pgxpool.Pool, ws string, ulxc int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO lxc_balances (workspace_id, balance) VALUES ($1,$2)
		 ON CONFLICT (workspace_id) DO UPDATE SET balance = EXCLUDED.balance`, ws, ulxc); err != nil {
		t.Fatal(err)
	}
}

func seamBalance(t *testing.T, store *economy.DualTokenStore, ws string) int64 {
	t.Helper()
	b, err := store.GetLXCBalance(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// usdToULXC mirrors settleReservation's conversion so the expected charge is exact ($0.10/LXC, µLXC ceil).
func usdToULXC(usd float64) int64 { return int64((usd/economy.LXCUSDValue)*1e6 + 0.5) }

// TestSettleChargesDeliveredNotHeld: a cache MISS routed/served at some real cost. The hold is large (an
// output-aware upper bound on gpt-4o); settling at the DELIVERED $0.012 charges 120000 µLXC, NOT the hold.
func TestSettleChargesDeliveredNotHeld(t *testing.T) {
	p, store, pool := seamProxy(t)
	ctx := context.Background()
	seamFund(t, pool, "ws", 100_000_000)
	rctx, blocked := p.agentReserveBlocks(ctx, "agent", "ws", "gpt-4o", "a fairly long prompt to hold against", "rq1", 4096)
	if blocked {
		t.Fatal("well-funded reserve must not block")
	}
	if b := seamBalance(t, store, "ws"); b >= 100_000_000 {
		t.Fatalf("hold did not debit: balance %d", b)
	}
	p.settleReservation(rctx, 0.012) // delivered $0.012
	if b := seamBalance(t, store, "ws"); b != 100_000_000-usdToULXC(0.012) {
		t.Fatalf("balance after settle = %d, want %d (charged the DELIVERED 0.012, not the hold)", b, 100_000_000-usdToULXC(0.012))
	}
}

// TestCheapRouteChargesCheapCost: reserve on the requested EXPENSIVE model (gpt-4o), settle at the CHEAP
// model's real cost (what actually served) — the routing saving reaches the customer's bill.
func TestCheapRouteChargesCheapCost(t *testing.T) {
	p, store, pool := seamProxy(t)
	ctx := context.Background()
	seamFund(t, pool, "ws", 100_000_000)
	rctx, _ := p.agentReserveBlocks(ctx, "agent", "ws", "gpt-4o", "prompt", "rq1", 4096)
	// Served by gpt-4o-mini at, say, 1000 in / 500 out — its ACTUAL cost.
	served := 0.15*1000/1e6 + 0.60*500/1e6 // gpt-4o-mini pricing
	p.settleReservation(rctx, served)
	if b := seamBalance(t, store, "ws"); b != 100_000_000-usdToULXC(served) {
		t.Fatalf("balance = %d, want %d (charged the cheap served cost)", b, 100_000_000-usdToULXC(served))
	}
}

// TestOwnCacheHitIsFree: reserve then resolve as an OWN cache hit (nil pooledHit) → full refund, balance
// unchanged. THE money invariant for a self-serve hit.
func TestOwnCacheHitIsFree(t *testing.T) {
	p, store, pool := seamProxy(t)
	ctx := context.Background()
	seamFund(t, pool, "ws", 100_000_000)
	rctx, _ := p.agentReserveBlocks(ctx, "agent", "ws", "gpt-4o", "prompt", "rq1", 4096)
	if funded := p.resolveCacheReservation(rctx, nil, "", nil); funded != 0 { // own hit → release → $0
		t.Fatalf("own cache hit funded=$%v, want 0 (free)", funded)
	}
	if b := seamBalance(t, store, "ws"); b != 100_000_000 {
		t.Fatalf("balance after own cache hit = %d, want 100000000 (FREE)", b)
	}
}

// TestCrossTenantHitChargesAvoidedCOGS: a pooled (cross-tenant) hit bills the requester avoided_COGS — the
// value received, derived from the served content — and resolve RETURNS that charge for the royalty seam.
func TestCrossTenantHitChargesAvoidedCOGS(t *testing.T) {
	p, store, pool := seamProxy(t)
	ctx := context.Background()
	seamFund(t, pool, "ws", 100_000_000)
	rctx, _ := p.agentReserveBlocks(ctx, "agent", "ws", "gpt-4o", "prompt", "rq1", 4096)
	prompt, served := "a requester prompt", []byte("a served cross-tenant cached response body")
	avoided := alerts.CostUSD("gpt-4o", len(prompt)/4, len(served)/4)
	wantCharge := int64(math.Ceil(avoided / economy.LXCUSDValue * 1e6)) // the exact conversion settleReservation uses
	funded := p.resolveCacheReservation(rctx, &poolroyalty.ServedHit{Model: "gpt-4o"}, prompt, served)
	if funded <= 0 {
		t.Fatalf("cross-tenant resolve must return the positive settled charge, got $%v", funded)
	}
	if b := seamBalance(t, store, "ws"); b != 100_000_000-wantCharge {
		t.Fatalf("balance after cross-tenant hit = %d, want %d (charged avoided_COGS $%v)", b, 100_000_000-wantCharge, avoided)
	}
}

// TestCeilingBlocksBeforeServe: an agent over its (tiny) ceiling is blocked at the HOLD, before serving —
// no debit. The ceiling stays enforced against the conservative reservation.
func TestCeilingBlocksBeforeServe(t *testing.T) {
	p, store, pool := seamProxy(t)
	ctx := context.Background()
	seamFund(t, pool, "ws", 100_000_000)
	if err := store.SetAgentCeiling(ctx, "agent", "ws", 100); err != nil { // 100 µLXC — any real hold exceeds it
		t.Fatal(err)
	}
	_, blocked := p.agentReserveBlocks(ctx, "agent", "ws", "gpt-4o", "prompt", "rq1", 4096)
	if !blocked {
		t.Fatal("over-ceiling agent must be BLOCKED before serving")
	}
	if b := seamBalance(t, store, "ws"); b != 100_000_000 {
		t.Fatalf("blocked hold must not debit: balance %d", b)
	}
}

// TestAndrewBug_ServedCostDissolvesPromptVariance: THE fix for Andrew's finding. Two requests whose pre-serve
// prompts differ (a long cold-distill prompt vs a short warm-distill one) hold DIFFERENT amounts — but when
// both are served at the SAME delivered cost, they are charged IDENTICALLY. Billing the served cost dissolves
// the joined-messages / distill-cache-warmth / estimate non-determinism entirely.
func TestAndrewBug_ServedCostDissolvesPromptVariance(t *testing.T) {
	p, store, pool := seamProxy(t)
	ctx := context.Background()
	seamFund(t, pool, "wsA", 100_000_000)
	seamFund(t, pool, "wsB", 100_000_000)

	longPrompt := ""
	for i := 0; i < 4000; i++ {
		longPrompt += "x"
	}
	rA, _ := p.agentReserveBlocks(ctx, "agentA", "wsA", "gpt-4o", longPrompt, "rqA", 4096)
	rB, _ := p.agentReserveBlocks(ctx, "agentB", "wsB", "gpt-4o", "short", "rqB", 4096)

	// The holds MUST differ (that is the bug's input) — different pre-serve prompt lengths.
	if seamBalance(t, store, "wsA") == seamBalance(t, store, "wsB") {
		t.Fatal("precondition: the two holds should differ (long vs short prompt)")
	}
	// Same DELIVERED cost → identical charge.
	const delivered = 0.003
	p.settleReservation(rA, delivered)
	p.settleReservation(rB, delivered)
	bA, bB := seamBalance(t, store, "wsA"), seamBalance(t, store, "wsB")
	if bA != bB {
		t.Fatalf("byte-different pre-serve prompts, SAME served cost → charges differ: wsA=%d wsB=%d", bA, bB)
	}
	if bA != 100_000_000-usdToULXC(delivered) {
		t.Fatalf("charge = %d, want %d (the delivered cost, independent of the hold)", 100_000_000-bA, usdToULXC(delivered))
	}
}
