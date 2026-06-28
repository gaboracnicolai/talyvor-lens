package povi_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/povi"
)

// P1 #9 invariant 4/5 end-to-end (real PG): a slash lowers R IN the slash tx, and the lowered R
// lowers the next bonded mint. Wires StakeManager (slash) + LedgerStore (reputation-gated mint) +
// ReputationStore (the sink) exactly as prod does when the flag is on.

type alwaysVerified struct{}

func (alwaysVerified) MayEarn(_ context.Context, _ pgx.Tx, _ string) (bool, error) { return true, nil }

func TestSlash_LowersReputation_LowersNextMint_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG slash-reputation test")
	}
	ctx := context.Background()
	// Private schema (Lens gated-test convention) — disjoint from the mining package's tables so a
	// parallel -p 2 run can't collide on shared table names.
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_slash_rep_test"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	const node, ws = "node-1", "ws-node-1"
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_slash_rep_test CASCADE`,
		`CREATE SCHEMA lens_slash_rep_test`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			locked_balance DOUBLE PRECISION NOT NULL DEFAULT 0, held_balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_earned DOUBLE PRECISION NOT NULL DEFAULT 0, lifetime_spent DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount DOUBLE PRECISION NOT NULL, balance_after DOUBLE PRECISION NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE povi_stakes (node_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, amount DOUBLE PRECISION NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'active', slashed_amount DOUBLE PRECISION NOT NULL DEFAULT 0,
			locked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), unbond_at TIMESTAMPTZ, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE reputation_events (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), annotator_id TEXT NOT NULL,
			kind TEXT NOT NULL, idem_key TEXT NOT NULL, delta DOUBLE PRECISION NOT NULL,
			reason JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (annotator_id, kind, idem_key))`,
		`CREATE INDEX idx_reputation_events_annotator ON reputation_events (annotator_id)`,
		// A slashable active stake of 100, fully locked in the balance row.
		`INSERT INTO lens_token_balances (workspace_id, locked_balance) VALUES ('ws-node-1', 100)`,
		`INSERT INTO povi_stakes (node_id, workspace_id, amount, status) VALUES ('node-1','ws-node-1',100,'active')`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}

	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(alwaysVerified{})              // U6 passes
	ledger.SetReputationGate(func() bool { return true }) // P1 #9 ON
	repStore := mining.NewReputationStore(pool)
	lookup := povi.NodeWorkspaceLookup(func(_ context.Context, n string) (string, error) { return ws, nil })
	sm := povi.NewStakeManager(povi.NewNodeStakeStore(pool), ledger, lookup, 100, 0, pool)
	sm.SetReputationSink(repStore) // wired only when the flag is on, as in main.go

	balance := func() float64 {
		var b float64
		_ = pool.QueryRow(ctx, `SELECT balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&b)
		return b
	}

	// 1. Baseline R=0.5 (no events) → f=1.0 → mint full base 10.
	if err := ledger.Credit(ctx, ws, 10, "receipt_mine_provisional", "d", map[string]interface{}{}); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	first := balance()
	if first != 10 {
		t.Fatalf("first mint balance %v, want 10 (R=0.5, f=1.0)", first)
	}

	// 2. Slash (challenge-fail) → reputation event appended IN the slash tx + stake burned.
	if _, err := sm.Slash(ctx, node, 0.10, "challenge_fail:req-1"); err != nil {
		t.Fatalf("slash: %v", err)
	}
	var slashDelta float64
	if err := pool.QueryRow(ctx,
		`SELECT delta FROM reputation_events WHERE annotator_id=$1 AND kind='slash'`, ws).Scan(&slashDelta); err != nil {
		t.Fatalf("slash reputation event not appended (invariant 4): %v", err)
	}
	if slashDelta != povi.SlashReputationDelta {
		t.Errorf("slash δ %v, want %v", slashDelta, povi.SlashReputationDelta)
	}
	var stakeAmt float64
	_ = pool.QueryRow(ctx, `SELECT amount FROM povi_stakes WHERE node_id=$1`, node).Scan(&stakeAmt)
	if stakeAmt != 90 {
		t.Errorf("stake after 10%% slash = %v, want 90 (burn happened)", stakeAmt)
	}

	// 3. Next mint: R = 0.5 − 0.10 = 0.40 → f = (0.40−0.35)/0.15 ≈ 0.333 → smaller credit than the first.
	if err := ledger.Credit(ctx, ws, 10, "receipt_mine_provisional", "d", map[string]interface{}{}); err != nil {
		t.Fatalf("second mint: %v", err)
	}
	second := balance() - first
	if second >= 10 || second <= 0 {
		t.Errorf("second mint credited %v, want a positive haircut < 10 (slash lowered R lowered the mint)", second)
	}
}
