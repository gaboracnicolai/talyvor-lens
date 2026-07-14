package royaltyhaircut

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mustTx opens a tx and registers its rollback as cleanup (errcheck-clean).
func mustTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// KE-2 oracle (real PG): Factor reads keel_findings and returns DriftFactor ONLY for a CURRENT hardened
// idiosyncratic drift — the fail-open predicate. These pin: hardened→reduce, ordinary→none, common_mode→none
// (SQL-enforced fail-open), stale-window→none, and no-finding→none.

func haircutPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG royaltyhaircut test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// The columns Factor reads (+ the NOT NULLs a keel_findings row carries). Matches migrations 0080/0081.
	if _, err := pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS keel_findings;
		CREATE TABLE keel_findings (
			id BIGSERIAL PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			unit TEXT NOT NULL,
			window_bucket BIGINT NOT NULL,
			deviation_sigma DOUBLE PRECISION NOT NULL,
			attribution TEXT NOT NULL,
			identity_key TEXT NOT NULL UNIQUE,
			mode TEXT NOT NULL DEFAULT 'ordinary',
			first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return pool
}

func seedFinding(t *testing.T, pool *pgxpool.Pool, ws, mode, attribution string, windowBucket int64, key string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO keel_findings (workspace_id, unit, window_bucket, deviation_sigma, attribution, identity_key, mode)
		 VALUES ($1,'model_used',$2,-3.0,$3,$4,$5)`,
		ws, windowBucket, attribution, key, mode); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestFactor_HardenedIdiosyncratic_Reduces(t *testing.T) {
	pool := haircutPool(t)
	ctx := context.Background()
	seedFinding(t, pool, "wsH", "hardened", "idiosyncratic", 100, "k1")
	tx := mustTx(t, pool)
	f, err := Factor(ctx, tx, "wsH", 90) // finding at 100 >= min 90
	if err != nil {
		t.Fatal(err)
	}
	if f != DriftFactor {
		t.Errorf("factor = %v, want DriftFactor %v (a current hardened idiosyncratic drift halves the royalty)", f, DriftFactor)
	}
}

func TestFactor_Ordinary_NoReduction(t *testing.T) {
	pool := haircutPool(t)
	ctx := context.Background()
	seedFinding(t, pool, "wsO", "ordinary", "idiosyncratic", 100, "k1") // NOT hardened
	tx := mustTx(t, pool)
	f, _ := Factor(ctx, tx, "wsO", 90)
	if f != 1.0 {
		t.Errorf("factor = %v, want 1.0 (an ordinary/contaminable-mean finding must NEVER reduce)", f)
	}
}

func TestFactor_CommonMode_NoReduction(t *testing.T) {
	pool := haircutPool(t)
	ctx := context.Background()
	// hardened never emits common_mode in reality; the SQL must still exclude it if present (fail-open).
	seedFinding(t, pool, "wsC", "hardened", "common_mode", 100, "k1")
	tx := mustTx(t, pool)
	f, _ := Factor(ctx, tx, "wsC", 90)
	if f != 1.0 {
		t.Errorf("factor = %v, want 1.0 (a common_mode finding must NEVER reduce — honest contributor in a shared regression)", f)
	}
}

func TestFactor_StaleWindow_NoReduction(t *testing.T) {
	pool := haircutPool(t)
	ctx := context.Background()
	seedFinding(t, pool, "wsS", "hardened", "idiosyncratic", 50, "k1") // old
	tx := mustTx(t, pool)
	f, _ := Factor(ctx, tx, "wsS", 90) // min 90 > 50 → stale, excluded
	if f != 1.0 {
		t.Errorf("factor = %v, want 1.0 (a drift older than the recency horizon must not penalise forever)", f)
	}
}

func TestFactor_NoFinding_NoReduction(t *testing.T) {
	pool := haircutPool(t)
	ctx := context.Background()
	tx := mustTx(t, pool)
	f, _ := Factor(ctx, tx, "wsNone", 90)
	if f != 1.0 {
		t.Errorf("factor = %v, want 1.0 (no finding → no haircut)", f)
	}
}
