package poolroyalty

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/mining"
)

// TestPhase4a_LiveEconomy_Acceptance is THE Phase-4a acceptance test: the whole
// closed-test economy (pool-royalty + distill + pattern), end to end on real PG,
// proving it works AND defends itself — all four scenarios in one proof.
func TestPhase4a_LiveEconomy_Acceptance(t *testing.T) {
	pool := linkageTestPool(t) // pool_royalty_mints + card/owner edge tables + balances(held) + ledger
	ctx := context.Background()
	// the extra tables the full economy needs
	linkExec(t, pool, `CREATE TABLE IF NOT EXISTS pool_royalty_adjudications (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(), flag_type TEXT NOT NULL, resolution_label TEXT NOT NULL,
		candidate_request_ids TEXT[] NOT NULL, revoked_request_ids TEXT[] NOT NULL, decided_by TEXT NOT NULL,
		outcome JSONB, decided_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
	linkExec(t, pool, `CREATE TABLE IF NOT EXISTS traffic_mint_holds (request_id TEXT NOT NULL, workspace_id TEXT NOT NULL,
		mint_type TEXT NOT NULL, minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held',
		finalize_after TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (request_id, workspace_id, mint_type))`)
	linkExec(t, pool, `CREATE TABLE IF NOT EXISTS routing_patterns (id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		workspace_id TEXT NOT NULL, feature_category TEXT NOT NULL, model_used TEXT NOT NULL, provider_used TEXT NOT NULL,
		input_token_range TEXT NOT NULL, output_quality DOUBLE PRECISION NOT NULL DEFAULT 0, latency_bucket TEXT NOT NULL,
		cache_hit_rate DOUBLE PRECISION NOT NULL DEFAULT 0, success_rate DOUBLE PRECISION NOT NULL DEFAULT 1,
		sample_count INT NOT NULL DEFAULT 1, rarity DOUBLE PRECISION NOT NULL DEFAULT 0, complexity_bucket TEXT NOT NULL DEFAULT '',
		opted_in BOOLEAN NOT NULL DEFAULT FALSE, earned BIGINT NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`)
	linkExec(t, pool, `CREATE TABLE IF NOT EXISTS pattern_mine_credits (id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		request_id TEXT NOT NULL, workspace_id TEXT NOT NULL, earned BIGINT NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), UNIQUE (request_id, workspace_id))`)

	ledger := mining.NewLedgerStore(pool)
	spendable := func(ws string) int64 {
		var b int64
		_ = pool.QueryRow(ctx, `SELECT COALESCE(balance,0) FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&b)
		return b
	}
	failClosed := func() bool { return true }

	// ── SCENARIO 1: honest cross-tenant reuse → pool-royalty HELD → examined → cleared → settles ──
	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	m.SetHoldbackWindow(time.Millisecond) // due ~immediately so the fail-closed sweeper can settle after clearing
	honest := seedHeldFor(t, m, ctx, "acc-honest", "opA", "reqB") // opA≠reqB, no shared identity ⇒ honest
	if spendable("opA") != 0 {
		t.Fatalf("honest mint must be HELD, not spendable yet")
	}
	ringDet := NewRingDetector(pool, "pool_royalty_mints")
	clearer := NewSettlementClearer(ringDet, pool, "pool_royalty_mints", failClosed, 24*time.Hour)
	if _, err := clearer.RunOnce(ctx); err != nil { // examine (clean) → held→cleared
		t.Fatalf("clearer: %v", err)
	}
	time.Sleep(4 * time.Millisecond)
	sweeper := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints")
	sweeper.SetSettleStatus("cleared") // fail-closed: settle only cleared
	if n, err := sweeper.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("finalize: n=%d err=%v, want 1 (the honest examined-clean mint settles)", n, err)
	}
	if got := spendable("opA"); got != micro(1.0) { // s=0.5 × avoided_COGS(2.0) = 1.0 LENS
		t.Fatalf("honest contributor spendable=%d, want %d µLENS (real settled royalty)", got, micro(1.0))
	}
	_ = honest

	// ── SCENARIO 2: transitive self-dealing ring (3 ws, one operator) → clawed back BEFORE settlement ──
	m.SetHoldbackWindow(time.Hour) // stays held while we adjudicate
	rAB := seedHeldFor(t, m, ctx, "acc-ring-ab", "wsA", "wsB")
	rBC := seedHeldFor(t, m, ctx, "acc-ring-bc", "wsB", "wsC")
	rCA := seedHeldFor(t, m, ctx, "acc-ring-ca", "wsC", "wsA") // transitive pair (A,C not directly linked)
	linkExec(t, pool, `INSERT INTO workspace_owner_links (workspace_id, owner_key) VALUES
		('wsA','opk1'),('wsB','opk1'),('wsB','opk2'),('wsC','opk2')`)
	auto := NewAutoAdjudicator(NewRingDetector(pool, "pool_royalty_mints"),
		NewAdjudicationWriter(pool, NewRevoker(pool, ledger)),
		func() bool { return true }, 24*time.Hour)
	revoked, err := auto.RunOnce(ctx)
	if err != nil || revoked != 3 {
		t.Fatalf("ring auto-adjudicate: revoked=%d err=%v, want 3", revoked, err)
	}
	for _, id := range []string{rAB, rBC, rCA} {
		var st string
		_ = pool.QueryRow(ctx, `SELECT status FROM pool_royalty_mints WHERE request_id=$1`, id).Scan(&st)
		if st != "revoked" {
			t.Errorf("ring mint %s status=%q, want revoked (clawed back before settlement)", id, st)
		}
	}
	for _, ws := range []string{"wsA", "wsB", "wsC"} {
		if spendable(ws) != 0 {
			t.Errorf("ring ws %s spendable=%d, want 0 (nothing became spendable)", ws, spendable(ws))
		}
	}

	// ── SCENARIO 3: single-party concentration farm on PATTERN → flagged → does NOT settle ──
	pm := mining.NewPatternMiner(ledger, pool)
	pm.SetHoldbackWindow(time.Millisecond)
	pm.SetEarnCap(0, time.Hour) // disable earn cap so the VELOCITY guard is what withholds (isolate the guard)
	for i := 0; i < 8; i++ {   // 8 pattern mints in the velocity window = a spike
		if err := pm.RecordPattern(ctx, patternWS(i), acceptancePattern(), true, fmt.Sprintf("acc-pat-farm-%d", i)); err != nil {
			t.Fatalf("pattern mint: %v", err)
		}
	}
	// honest low-velocity workspace: 2
	_ = pm.RecordPattern(ctx, "ws_pat_hon", acceptancePattern(), true, "acc-pat-h1")
	_ = pm.RecordPattern(ctx, "ws_pat_hon", acceptancePattern(), true, "acc-pat-h2")
	det := mining.NewSinglePartyConcentrationDetector(pool, mining.TypePatternMine, 5, 24*time.Hour)
	pClearer := mining.NewTrafficSettlementClearer(det, pool, failClosed, 24*time.Hour)
	if _, err := pClearer.RunOnce(ctx); err != nil {
		t.Fatalf("pattern clearer: %v", err)
	}
	time.Sleep(4 * time.Millisecond)
	tsw := mining.NewTrafficMintSweeper(pool, ledger)
	tsw.SetSettleStatus("cleared")
	if _, err := tsw.RunOnce(ctx); err != nil {
		t.Fatalf("traffic sweep: %v", err)
	}
	if spendable("ws_pat_hon") != micro(0.002) { // 2 × 0.001 base
		t.Errorf("honest pattern ws spendable=%d, want %d (settled)", spendable("ws_pat_hon"), micro(0.002))
	}
	if spendable("ws_pat_farm") != 0 { // the farm workspace: withheld
		t.Errorf("pattern farm spendable=%d, want 0 (flagged velocity spike ⇒ withheld)", spendable("ws_pat_farm"))
	}

	// ── SCENARIO 4: the U6 rate cap bounds a high-volume workspace at 1000 LENS/24h ──
	capLedger := mining.NewLedgerStore(pool)
	capLedger.SetMintRateCap(micro(1000), 24*time.Hour) // 1000 LENS/24h
	tx, _ := pool.Begin(ctx)
	// two big mints: 700 + 400 = 1100 > 1000 ⇒ the second must be denied.
	if err := capLedger.CreditHeldTx(ctx, tx, "ws_cap", micro(700), TypePoolRoyaltyHeld, "cap1", nil); err != nil {
		t.Fatalf("first mint (700) must succeed: %v", err)
	}
	err2 := capLedger.CreditHeldTx(ctx, tx, "ws_cap", micro(400), TypePoolRoyaltyHeld, "cap2", nil)
	if err2 == nil {
		t.Error("the rate cap must DENY the mint that pushes a workspace over 1000 LENS/24h")
	}
	_ = tx.Rollback(ctx)
}

func patternWS(i int) string { return "ws_pat_farm" } // all 8 to one workspace = the spike

func acceptancePattern() mining.RoutingPattern {
	return mining.RoutingPattern{
		FeatureCategory: "code", ModelUsed: "claude", ProviderUsed: "anthropic",
		InputTokenRange: mining.InputBucketMedium, LatencyBucket: mining.LatencyFast,
		OutputQuality: 0.85, SuccessRate: 1.0, SampleCount: 1,
	}
}
