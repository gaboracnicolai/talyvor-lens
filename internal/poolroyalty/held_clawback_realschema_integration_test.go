package poolroyalty

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// Phase-3 Item 2 — a revoker for EVERY held table, proven against the REAL
// production column types. Prod minted_amount is BIGINT µLENS for ALL the mint
// tables: migration 0082 (SEC-2) ALTERed the six family tables from their
// pre-0082 DOUBLE CREATE to BIGINT (0082:84-89), and traffic_mint_holds was born
// BIGINT (0090). (Phase-4-PRE fix: this harness previously declared DOUBLE on the
// stale belief that prod was DOUBLE — it passed only via pgx int64↔double
// coercion, masking that it exercised a type prod does not use. Now it matches.)

// familyRevokeHarness builds a dedicated-schema pool with the ledger tables and
// ONE family mint table whose minted_amount is BIGINT µLENS (as in prod, post-0082).
func familyRevokeHarness(t *testing.T, mintTable string) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG held-clawback test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_heldclawback_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ddls := []string{
		`DROP SCHEMA IF EXISTS lens_heldclawback_test CASCADE`,
		`CREATE SCHEMA lens_heldclawback_test`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		// The mint table — minted_amount BIGINT µLENS, matching the real migration (0082).
		`CREATE TABLE ` + mintTable + ` (request_id TEXT PRIMARY KEY, contributor_workspace_id TEXT NOT NULL,
			minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held',
			finalize_after TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	}
	for _, ddl := range ddls {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool, mining.NewLedgerStore(pool)
}

func heldClawbackBal(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var b int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT held_balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&b)
	return b
}

// seedFamilyHeldMint credits held balance (the mint effect) AND writes the claim
// row with the real BIGINT µLENS minted_amount, so a revoke exercises the real path.
func seedFamilyHeldMint(t *testing.T, pool *pgxpool.Pool, ledger *mining.LedgerStore, mintTable, reqID, contributor, heldType string, amount int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.CreditHeldTx(ctx, tx, contributor, amount, heldType, "seed held mint", nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("seed CreditHeldTx: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO `+mintTable+` (request_id, contributor_workspace_id, minted_amount, status, finalize_after)
		 VALUES ($1,$2,$3,'held', now() + interval '1 hour')`, reqID, contributor, amount); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("seed claim row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestHeldClawback_FamilyTables_RevocableBeforeFinalize_RealSchema proves the
// generic Revoker reverses a held mint in EACH P-o-I family table (real DOUBLE
// minted_amount): status held→revoked and the held balance is burned. Today no
// revoker is constructed for these tables; this pins that NewRevokerForTable
// reaches them against the production column type.
func TestHeldClawback_FamilyTables_RevocableBeforeFinalize_RealSchema(t *testing.T) {
	cases := []struct {
		table    string
		heldType string
	}{
		{"eval_contribution_mints", mining.TypeEvalContributionHeld},
		{"routing_prediction_mints", mining.TypeRoutingPredictionHeld},
		{"node_latency_mints", mining.TypeLatencyLocalityHeld},
		{"confidential_compute_mints", mining.TypeConfidentialComputeHeld},
	}
	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			pool, ledger := familyRevokeHarness(t, c.table)
			ctx := context.Background()
			// The harness schema must exercise the REAL production type. Prod
			// minted_amount is BIGINT µLENS (migration 0082 converted the six family
			// tables from their pre-0082 DOUBLE CREATE). Assert it here so the test
			// CATCHES a type divergence instead of coercing an int64 through a DOUBLE
			// column and passing regardless.
			var mintedType string
			if err := pool.QueryRow(ctx,
				`SELECT data_type FROM information_schema.columns
				 WHERE table_schema='lens_heldclawback_test' AND table_name=$1 AND column_name='minted_amount'`,
				c.table).Scan(&mintedType); err != nil {
				t.Fatalf("read minted_amount type: %v", err)
			}
			if mintedType != "bigint" {
				t.Fatalf("%s.minted_amount is %q, want bigint — the harness must match prod (0082 µLENS), not the stale pre-0082 DOUBLE", c.table, mintedType)
			}
			seedFamilyHeldMint(t, pool, ledger, c.table, "rid-1", "wsN", c.heldType, 50_000)
			if held := heldClawbackBal(t, pool, "wsN"); held != 50_000 {
				t.Fatalf("pre-revoke held=%d, want 50000", held)
			}

			rev := NewRevokerForTable(pool, ledger, c.table)
			rep, _ := rev.RevokeHeldMints(ctx, []string{"rid-1"})
			if rep.Outcomes["rid-1"] != OutcomeRevoked {
				t.Fatalf("%s: revoke outcome %v, want revoked (the held mint must be reversible before finalize)", c.table, rep.Outcomes["rid-1"])
			}
			var status string
			_ = pool.QueryRow(ctx, `SELECT status FROM `+c.table+` WHERE request_id='rid-1'`).Scan(&status)
			if status != "revoked" {
				t.Fatalf("%s: status=%q, want revoked", c.table, status)
			}
			if held := heldClawbackBal(t, pool, "wsN"); held != 0 {
				t.Fatalf("%s: post-revoke held=%d, want 0 (burned)", c.table, held)
			}
		})
	}
}
