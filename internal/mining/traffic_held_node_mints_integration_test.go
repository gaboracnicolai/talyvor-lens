package mining

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase-3 Item 1 — the compute + embedding NODE mints must land HELD (not
// spendable), mirroring cache. Before Phase 3 both producers called CreditOnce
// (direct-to-spendable, no held state, no clawback surface) while the
// traffic_holds.go / main.go comments claimed all three were held. These
// real-PG RED proofs pin the corrected routing: a served node mint lands HELD,
// stays out of spendable until the finalize sweeper settles it, and the counted
// base type is what enters supply at settlement.

type nodeMintVerifier struct{ verified map[string]bool }

func (v nodeMintVerifier) MayEarn(_ context.Context, _ pgx.Tx, ws string) (bool, error) {
	return v.verified[ws], nil
}

// trafficHeldHarness builds a dedicated-schema real-PG pool with the ledger
// tables + the node tables the compute/embedding producers look up, and vouches
// the node-owner workspace so the U6 floor passes.
func trafficHeldHarness(t *testing.T, ownerWS string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG traffic-held node-mint test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_traffic_held_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_traffic_held_test CASCADE`,
		`CREATE SCHEMA lens_traffic_held_test`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE mint_idempotency (request_id TEXT NOT NULL, workspace_id TEXT NOT NULL,
			mint_type TEXT NOT NULL, amount BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (request_id, workspace_id, mint_type))`,
		`CREATE TABLE traffic_mint_holds (request_id TEXT NOT NULL, workspace_id TEXT NOT NULL, mint_type TEXT NOT NULL,
			minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (request_id, workspace_id, mint_type))`,
		`CREATE TABLE inference_nodes (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, url TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '', models TEXT[] NOT NULL DEFAULT '{}', gpu_type TEXT NOT NULL DEFAULT '',
			max_concurrent INT NOT NULL DEFAULT 1, price_per_token DOUBLE PRECISION NOT NULL DEFAULT 0,
			active BOOLEAN NOT NULL DEFAULT true, verified BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE embedding_nodes (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, url TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '', dimensions INT NOT NULL DEFAULT 0, max_batch INT NOT NULL DEFAULT 1,
			speed_tps DOUBLE PRECISION NOT NULL DEFAULT 0, active BOOLEAN NOT NULL DEFAULT true,
			verified BOOLEAN NOT NULL DEFAULT true, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE node_metrics (node_id TEXT PRIMARY KEY, requests_served BIGINT NOT NULL DEFAULT 0,
			tokens_served BIGINT NOT NULL DEFAULT 0, avg_latency_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
			last_active_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func heldBalance(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var b int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT held_balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&b)
	return b
}

func spendableBalance(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var b int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&b)
	return b
}

// RED (Phase-3 Item 1): a compute (node) mint currently lands DIRECT-to-spendable
// via CreditOnce. It must land HELD like cache. Proof: after a served cross-tenant
// request the node owner's HELD balance carries the earning and SPENDABLE stays 0,
// a traffic_mint_holds row exists (status='held'), and only the finalize sweeper
// moves it to spendable + supply. Before the fix: spendable == earning, held == 0,
// and no traffic_mint_holds row (this test fails on all three).
func TestComputeMint_LandsHeld_FinalizesAfterWindow_Integration(t *testing.T) {
	pool := trafficHeldHarness(t, "node-owner")
	ctx := context.Background()
	ledger := NewLedgerStore(pool)
	ledger.SetMintVerifier(nodeMintVerifier{verified: map[string]bool{"node-owner": true}})
	miner := NewComputeMiner(ledger, pool)
	miner.SetHoldbackWindow(time.Millisecond) // due almost immediately for the sweep

	if _, err := pool.Exec(ctx,
		`INSERT INTO inference_nodes (id, workspace_id, gpu_type, models) VALUES ('n1','node-owner','rtx4090','{}')`); err != nil {
		t.Fatal(err)
	}
	want := EarningRate("rtx4090", 1000) // the served-request earning

	if err := miner.RecordServedRequest(ctx, "n1", "requester-ws", "creq-1", 1000, 5); err != nil {
		t.Fatalf("RecordServedRequest: %v", err)
	}
	if held := heldBalance(t, pool, "node-owner"); held != want {
		t.Fatalf("node-owner held=%d, want %d — a compute mint must land HELD (was: minted spendable immediately)", held, want)
	}
	if sp := spendableBalance(t, pool, "node-owner"); sp != 0 {
		t.Fatalf("node-owner spendable=%d, want 0 — a held mint is NOT spendable before finalize", sp)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM traffic_mint_holds WHERE request_id='creq-1'`).Scan(&status); err != nil {
		t.Fatalf("no traffic_mint_holds row for compute mint: %v", err)
	}
	if status != "held" {
		t.Fatalf("compute hold status=%q, want held", status)
	}

	time.Sleep(5 * time.Millisecond)
	if n, err := NewTrafficMintSweeper(pool, ledger).RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("sweep finalized n=%d err=%v, want 1", n, err)
	}
	if sp := spendableBalance(t, pool, "node-owner"); sp != want {
		t.Fatalf("node-owner spendable after finalize=%d, want %d", sp, want)
	}
	if supply, _ := ledger.GetTotalSupply(ctx); supply != want {
		t.Fatalf("supply=%d, want %d (finalized compute mint counted as compute_mine)", supply, want)
	}
}

// RED (Phase-3 Item 1): the embedding node mint — identical routing proof.
func TestEmbeddingMint_LandsHeld_FinalizesAfterWindow_Integration(t *testing.T) {
	pool := trafficHeldHarness(t, "node-owner")
	ctx := context.Background()
	ledger := NewLedgerStore(pool)
	ledger.SetMintVerifier(nodeMintVerifier{verified: map[string]bool{"node-owner": true}})
	miner := NewEmbeddingMiner(ledger, pool)
	miner.SetHoldbackWindow(time.Millisecond)

	if _, err := pool.Exec(ctx,
		`INSERT INTO embedding_nodes (id, workspace_id, model, dimensions) VALUES ('e1','node-owner','text-embedding-3-small',1536)`); err != nil {
		t.Fatal(err)
	}
	want := EmbeddingEarningRate("text-embedding-3-small", 100)

	if err := miner.RecordEmbeddingsServed(ctx, "e1", "requester-ws", "ereq-1", 100); err != nil {
		t.Fatalf("RecordEmbeddingsServed: %v", err)
	}
	if held := heldBalance(t, pool, "node-owner"); held != want {
		t.Fatalf("node-owner held=%d, want %d — an embedding mint must land HELD (was: minted spendable immediately)", held, want)
	}
	if sp := spendableBalance(t, pool, "node-owner"); sp != 0 {
		t.Fatalf("node-owner spendable=%d, want 0 — a held mint is NOT spendable before finalize", sp)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM traffic_mint_holds WHERE request_id='ereq-1'`).Scan(&status); err != nil {
		t.Fatalf("no traffic_mint_holds row for embedding mint: %v", err)
	}
	if status != "held" {
		t.Fatalf("embedding hold status=%q, want held", status)
	}

	time.Sleep(5 * time.Millisecond)
	if n, err := NewTrafficMintSweeper(pool, ledger).RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("sweep finalized n=%d err=%v, want 1", n, err)
	}
	if sp := spendableBalance(t, pool, "node-owner"); sp != want {
		t.Fatalf("node-owner spendable after finalize=%d, want %d", sp, want)
	}
	if supply, _ := ledger.GetTotalSupply(ctx); supply != want {
		t.Fatalf("supply=%d, want %d (finalized embedding mint counted as embedding_mine)", supply, want)
	}
}

// The GPU multiplier still applies through the held path (an h100 node earns 3×
// an rtx4090 for the same tokens) — the credit that lands HELD carries the scaled
// amount. (Preserves the coverage the former mock TestRecordServedRequest_UsesGPUMultiplier had.)
func TestComputeMint_GPUMultiplier_LandsHeld_Integration(t *testing.T) {
	pool := trafficHeldHarness(t, "node-owner")
	ctx := context.Background()
	ledger := NewLedgerStore(pool)
	ledger.SetMintVerifier(nodeMintVerifier{verified: map[string]bool{"node-owner": true}})
	miner := NewComputeMiner(ledger, pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO inference_nodes (id, workspace_id, gpu_type) VALUES ('h100n','node-owner','h100')`); err != nil {
		t.Fatal(err)
	}
	want := EarningRate("h100", 1000)
	if want != 3*EarningRate("rtx4090", 1000) {
		t.Fatalf("h100 rate %d must be 3× the rtx4090 rate %d", want, EarningRate("rtx4090", 1000))
	}
	if err := miner.RecordServedRequest(ctx, "h100n", "requester-ws", "h100-req", 1000, 5); err != nil {
		t.Fatalf("RecordServedRequest: %v", err)
	}
	if held := heldBalance(t, pool, "node-owner"); held != want {
		t.Fatalf("h100 held=%d, want %d (3× multiplier through the held path)", held, want)
	}
	if sp := spendableBalance(t, pool, "node-owner"); sp != 0 {
		t.Fatalf("h100 spendable=%d, want 0 (held, not spendable)", sp)
	}
}

// The embedding model tier still applies through the held path (e5-large's
// medium tier). (Preserves the coverage the former mock TestRecordEmbeddingsServed_CreditsOwner had.)
func TestEmbeddingMint_ModelTier_LandsHeld_Integration(t *testing.T) {
	pool := trafficHeldHarness(t, "node-owner")
	ctx := context.Background()
	ledger := NewLedgerStore(pool)
	ledger.SetMintVerifier(nodeMintVerifier{verified: map[string]bool{"node-owner": true}})
	miner := NewEmbeddingMiner(ledger, pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO embedding_nodes (id, workspace_id, model, dimensions) VALUES ('e5','node-owner','e5-large',1024)`); err != nil {
		t.Fatal(err)
	}
	want := EmbeddingEarningRate("e5-large", 2000)
	if err := miner.RecordEmbeddingsServed(ctx, "e5", "requester-ws", "e5-req", 2000); err != nil {
		t.Fatalf("RecordEmbeddingsServed: %v", err)
	}
	if held := heldBalance(t, pool, "node-owner"); held != want {
		t.Fatalf("e5-large held=%d, want %d (model-tier rate through the held path)", held, want)
	}
	if sp := spendableBalance(t, pool, "node-owner"); sp != 0 {
		t.Fatalf("e5-large spendable=%d, want 0 (held, not spendable)", sp)
	}
}
