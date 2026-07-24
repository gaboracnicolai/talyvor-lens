package poolroyalty

// THE DISTILL FUNDING INVARIANT (real PG). The async DistillMinter sweeper mints a reuse royalty off
// distill_royalty_basis. Before this fix it minted s × avoided_COGS — what B AVOIDED — regardless of what
// B was actually CHARGED. So a reuse funded by nothing minted a real royalty, exactly the pool-royalty hole
// (#351) in a different shape. This pairs BOTH ledgers in one assertion — the consumer's LXC debit and the
// contributor's LENS credit — because that pairing IS the invariant:
//
//   · consumer charged $0 (no recorded settled_charge) → contributor minted $0
//   · consumer charged $10                             → contributor minted ≤ $5 (s=0.5), off the CHARGE
//
// The mint reads the SETTLED charge recorded on the basis at serve time, never the avoided_COGS figure.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
)

// distillFundingHarness wires the consumer's LXC economy (reservation settle → real charge) AND the
// contributor's LENS ledger (the distill royalty), plus distill_royalty_basis WITH the settled_charge_usd
// column the fix reads.
func distillFundingHarness(t *testing.T) (*pgxpool.Pool, *mining.LedgerStore, *economy.DualTokenStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG distill funding test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP VIEW IF EXISTS distill_royalty_margin`,
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`DROP TABLE IF EXISTS distill_royalty_mints`,
		`DROP TABLE IF EXISTS distill_royalty_basis`,
		`DROP TABLE IF EXISTS workspaces`,
		`DROP TABLE IF EXISTS lxc_purchases`,
		`DROP TABLE IF EXISTS lxc_balances`,
		`DROP TABLE IF EXISTS lxc_ledger`,
		`DROP TABLE IF EXISTS agent_lxc_subbudgets`,
		`DROP TABLE IF EXISTS lxc_reservations`,
		// consumer (LXC) side
		`CREATE TABLE lxc_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			lifetime_minted BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE lxc_ledger (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE agent_lxc_subbudgets (scoped_key_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL,
			ceiling_lxc BIGINT NOT NULL DEFAULT 50000000, spent_lxc BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE lxc_reservations (reservation_id TEXT PRIMARY KEY, scoped_key_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL, held_ulxc BIGINT NOT NULL, settled_ulxc BIGINT,
			status TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held','settled','released')),
			requested_model TEXT, request_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), resolved_at TIMESTAMPTZ)`,
		// contributor (LENS) side + basis WITH the settled_charge_usd column (0105)
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE distill_royalty_basis (owner_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL,
			content_hash TEXT NOT NULL, avoided_cogs_usd DOUBLE PRECISION NOT NULL, settled_charge_usd DOUBLE PRECISION,
			vision_model TEXT NOT NULL, vision_input_tokens INTEGER NOT NULL, vision_output_tokens INTEGER NOT NULL,
			captured_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (owner_workspace_id, requester_workspace_id, content_hash))`,
		`CREATE TABLE distill_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE,
			contributor_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL, content_hash TEXT NOT NULL,
			avoided_cogs_usd DOUBLE PRECISION NOT NULL, minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held',
			finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL DEFAULT 0)`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New())
	return pool, ledger, economy.NewDualTokenStore(nil, pool, nil)
}

// chargeConsumerLXC drives a REAL reservation settle so the consumer's LXC is debited exactly chargeUSD,
// and returns the settled USD — the same number that must fund the royalty.
func chargeConsumerLXC(t *testing.T, store *economy.DualTokenStore, pool *pgxpool.Pool, consumer string, chargeUSD float64) float64 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO lxc_balances (workspace_id, balance) VALUES ($1, 1000000000)
		ON CONFLICT (workspace_id) DO UPDATE SET balance = 1000000000`, consumer); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgentCeiling(ctx, "agent-"+consumer, consumer, 1_000_000_000); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveLXCForAgent(ctx, "agent-"+consumer, consumer, "res-"+consumer, 500_000_000, economy.AgentDebitMeta{}); err != nil {
		t.Fatal(err)
	}
	settled, err := store.SettleLXCReservation(ctx, "res-"+consumer, int64(chargeUSD/economy.LXCUSDValue*1e6+0.5), economy.AgentDebitMeta{})
	if err != nil {
		t.Fatal(err)
	}
	return float64(settled) * economy.LXCUSDValue / 1e6
}

func distillEarnVerify(t *testing.T, pool *pgxpool.Pool, ws string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, earn_verified) VALUES ($1, true) ON CONFLICT (id) DO UPDATE SET earn_verified = true`, ws); err != nil {
		t.Fatal(err)
	}
}

func distillConsumerLXC(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var b int64
	_ = pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT balance FROM lxc_balances WHERE workspace_id=$1),0)`, ws).Scan(&b)
	return b
}

func distillContributorHeld(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var h int64
	_ = pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT held_balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&h)
	return h
}

// seedBasisWithCharge inserts one basis row with an explicit avoided_COGS and (nullable) settled charge.
// chargeUSD < 0 ⇒ NULL settled_charge (an UNFUNDED reuse — a plain-key/unmetered/historical serve).
func seedBasisWithCharge(t *testing.T, pool *pgxpool.Pool, owner, requester, hash string, avoidedUSD, chargeUSD float64) {
	t.Helper()
	var charge interface{}
	if chargeUSD >= 0 {
		charge = chargeUSD
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO distill_royalty_basis (owner_workspace_id, requester_workspace_id, content_hash, avoided_cogs_usd, settled_charge_usd, vision_model, vision_input_tokens, vision_output_tokens)
		 VALUES ($1,$2,$3,$4,$5,'gpt-4o-mini',500,20)`, owner, requester, hash, avoidedUSD, charge); err != nil {
		t.Fatalf("seed basis: %v", err)
	}
}

// (A) UNFUNDED reuse: the basis exists (avoided $10) but NO consumer charge was recorded — a plain key, an
// unmetered lane, a failed settle, or a historical row from before the fix. The contributor must be minted
// ZERO. BOTH ledgers asserted together.
func TestDistillFunding_UnfundedReuse_MintsZero_Integration(t *testing.T) {
	pool, ledger, _ := distillFundingHarness(t)
	ctx := context.Background()
	const contributor, consumer = "wsA", "wsB"
	distillEarnVerify(t, pool, contributor) // contributor CAN earn — so only the funding gate can stop the mint
	seedBasisWithCharge(t, pool, contributor, consumer, "h1", 10.0, -1 /* NULL charge */)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	if n, err := m.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	} else if n != 0 {
		t.Fatalf("minted %d relationships, want 0 (an UNFUNDED reuse mints nothing)", n)
	}
	if held := distillContributorHeld(t, pool, contributor); held != 0 {
		t.Fatalf("contributor LENS held = %d, want 0 — an unfunded distill reuse is minting from nothing", held)
	}
	if bal := distillConsumerLXC(t, pool, consumer); bal != 0 {
		t.Fatalf("consumer LXC = %d, want 0 (never charged)", bal)
	}
}

// (B) FUNDED reuse: the consumer is charged exactly $10 (real settle). The basis records avoided $20 (what B
// avoided) but the mint must follow the $10 CHARGE — contributor minted s × $10 = $5 = 50,000,000 µLENS,
// NOT s × $20. BOTH ledgers asserted, plus the pairing invariant mint-value ≤ charge.
func TestDistillFunding_Charged10_MintsOffChargeNotAvoided_Integration(t *testing.T) {
	pool, ledger, store := distillFundingHarness(t)
	ctx := context.Background()
	const contributor, consumer = "wsA", "wsB"
	distillEarnVerify(t, pool, contributor)

	charge := chargeConsumerLXC(t, store, pool, consumer, 10.0) // consumer LXC debited exactly $10
	if charge != 10.0 {
		t.Fatalf("settled charge = $%v, want $10", charge)
	}
	if bal := distillConsumerLXC(t, pool, consumer); bal != 1_000_000_000-100_000_000 {
		t.Fatalf("consumer LXC after $10 charge = %d, want %d", bal, 1_000_000_000-100_000_000)
	}
	// avoided $20, settled charge $10 → the mint MUST be s × $10, not s × $20.
	seedBasisWithCharge(t, pool, contributor, consumer, "h1", 20.0, charge)

	m := NewDistillMinter(pool, ledger, 0.5, func() bool { return true })
	if n, err := m.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce: n=%d err=%v, want 1", n, err)
	}
	held := distillContributorHeld(t, pool, contributor)
	if held != 50_000_000 {
		t.Fatalf("contributor LENS held = %d, want 50_000_000 (s × $10 CHARGE at the peg, NOT s × $20 avoided)", held)
	}
	// PAIRING INVARIANT: the royalty's worth never exceeds what the consumer paid.
	mintValueUSD := float64(held) / 1e6 * economy.LXCUSDValue
	if mintValueUSD > charge+1e-9 {
		t.Fatalf("ROYALTY EXCEEDS CHARGE: mint $%v of value > consumer charge $%v", mintValueUSD, charge)
	}
	if mintValueUSD != 5.0 {
		t.Fatalf("mint value = $%v, want $5 (half of the $10 charge)", mintValueUSD)
	}}
