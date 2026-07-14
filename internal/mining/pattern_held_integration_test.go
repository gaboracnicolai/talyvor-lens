package mining

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// patternHeldPool builds the pattern earn schema PLUS held_balance + the generic
// traffic_mint_holds table (where the Phase-4a pattern held mint lands).
func patternHeldPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG pattern-held test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS routing_patterns, lens_token_ledger, lens_token_balances, pattern_mine_credits, traffic_mint_holds`,
		`CREATE TABLE routing_patterns (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			feature_category TEXT NOT NULL, model_used TEXT NOT NULL, provider_used TEXT NOT NULL,
			input_token_range TEXT NOT NULL, output_quality DOUBLE PRECISION NOT NULL DEFAULT 0,
			latency_bucket TEXT NOT NULL, cache_hit_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
			success_rate DOUBLE PRECISION NOT NULL DEFAULT 1, sample_count INT NOT NULL DEFAULT 1,
			rarity DOUBLE PRECISION NOT NULL DEFAULT 0, complexity_bucket TEXT NOT NULL DEFAULT '', opted_in BOOLEAN NOT NULL DEFAULT FALSE,
			earned BIGINT NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, locked_balance BIGINT NOT NULL DEFAULT 0,
			lifetime_earned BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT, metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE pattern_mine_credits (id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			request_id TEXT NOT NULL, workspace_id TEXT NOT NULL, earned BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), UNIQUE (request_id, workspace_id))`,
		`CREATE TABLE traffic_mint_holds (request_id TEXT NOT NULL, workspace_id TEXT NOT NULL, mint_type TEXT NOT NULL,
			minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (request_id, workspace_id, mint_type))`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func heldBal(t *testing.T, pool *pgxpool.Pool, ws string) (held, spendable int64) {
	t.Helper()
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE(held_balance,0), COALESCE(balance,0) FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&held, &spendable)
	return
}

// TestPatternMint_LandsHeld_Settles_CapsHold is the Phase-4a Item 1 RED-first
// proof: rerouting pattern CreditTx→CreditOnceHeld must make the mint land HELD
// (examinable + settle-after-window) WITHOUT loosening the rarity cap or the earn
// cap.
func TestPatternMint_LandsHeld_Settles_CapsHold(t *testing.T) {
	pool := patternHeldPool(t)
	ctx := context.Background()
	ledger := newLedgerStore(pool)
	miner := NewPatternMiner(ledger, pool)
	miner.SetHoldbackWindow(time.Millisecond) // due almost immediately for the settle step

	// ── (1) a pattern mint LANDS HELD (rarity 0 ⇒ earned == PatternBaseRate) ──
	want := PatternEarning(RoutingPattern{Rarity: 0}) // the exact rarity-capped amount
	if err := miner.RecordPattern(ctx, "ws_h", earnPattern(), true, "ph-1"); err != nil {
		t.Fatalf("RecordPattern: %v", err)
	}
	held, spend := heldBal(t, pool, "ws_h")
	if held != want || spend != 0 {
		t.Fatalf("after mint: held=%d spendable=%d, want held=%d spendable=0 (must land HELD, not spendable)", held, spend, want)
	}
	var st string
	var amt int64
	if err := pool.QueryRow(ctx, `SELECT status, minted_amount FROM traffic_mint_holds WHERE request_id='ph-1' AND mint_type='pattern_mine'`).Scan(&st, &amt); err != nil {
		t.Fatalf("traffic_mint_holds row for pattern: %v", err)
	}
	if st != "held" || amt != want {
		t.Fatalf("traffic hold: status=%q amount=%d, want held/%d", st, amt, want)
	}

	// ── (2) settles after the window ──
	time.Sleep(5 * time.Millisecond)
	if n, err := NewTrafficMintSweeper(pool, ledger).RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", n, err)
	}
	held, spend = heldBal(t, pool, "ws_h")
	if held != 0 || spend != want {
		t.Fatalf("after settle: held=%d spendable=%d, want 0/%d", held, spend, want)
	}
	if supply, _ := ledger.GetTotalSupply(ctx); supply != want {
		t.Fatalf("supply=%d, want %d (pattern_mine counted at finalize)", supply, want)
	}

	// ── (3) the EARN CAP still bounds (post-reroute): cap=3, record 5 ⇒ only 3 credited HELD ──
	capMiner := NewPatternMiner(ledger, pool)
	capMiner.SetHoldbackWindow(time.Hour) // stays held so we read the held total
	capMiner.SetEarnCap(3, time.Hour)
	for i := 0; i < 5; i++ {
		_ = capMiner.RecordPattern(ctx, "ws_cap", earnPattern(), true, "cap-"+string(rune('a'+i)))
	}
	heldCap, spendCap := heldBal(t, pool, "ws_cap")
	if heldCap != 3*want || spendCap != 0 {
		t.Fatalf("earn cap: held=%d spendable=%d, want held=%d (exactly 3 of 5 credited — the cap must still bound the HELD credits)", heldCap, spendCap, 3*want)
	}
}
