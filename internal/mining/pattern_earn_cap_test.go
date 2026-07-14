package mining

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pashagolub/pgxmock/v4"
)

// (expectApplyTx — the 4 in-tx balance queries CreditTx issues, no Begin/Commit
// — is shared from annotation_mining_test.go: signature
// (mock, ws, startBal, startEarned, startSpent, delta, expBal, expEarned, expSpent).)

func earnPattern() RoutingPattern {
	return RoutingPattern{
		FeatureCategory: "code", ModelUsed: "claude", ProviderUsed: "anthropic",
		InputTokenRange: InputBucketMedium, LatencyBucket: LatencyFast,
		OutputQuality: 0.85, CacheHitRate: 0.0, SuccessRate: 1.0, SampleCount: 1,
	}
}

// expectScoreRarity mocks the (feature-excluded, 5-arg) rarity COUNT for earnPattern.
func expectScoreRarity(mock pgxmock.PgxPoolIface, ws string, n int) {
	mock.ExpectQuery("SELECT COUNT\\(DISTINCT workspace_id\\)").
		WithArgs(ws, "claude", "anthropic", InputBucketMedium, LatencyFast).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(n))
}

// OVER-CAP atomic block: cap COUNT n=3 > cap 2 → return nil WITHOUT commit;
// deferred rollback discards the routing_patterns row AND the credit; no
// MintedTokens. (rarity floors to 0.0 with n=0 corroboration → earned = base 0.001.)
func TestRecordPattern_OverCap_AtomicBlock(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	miner.SetEarnCap(2, time.Hour)
	expectScoreRarity(mock, "ws_e", 0) // n=0 → rarity 0.0 → earned base micro(0.001)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO pattern_mine_credits").
		WithArgs("req-over", "ws_e", micro(0.001)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1)) // claim taken (not a dup)
	mock.ExpectQuery("INSERT INTO routing_patterns").
		WithArgs("ws_e", "code", "claude", "anthropic", InputBucketMedium,
			0.85, LatencyFast, 0.0, 1.0, 1, 0.0, "", true, micro(0.001)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow("p1", time.Now()))
	// Phase-4a Item 1: the credit is now HELD (CreditHeldTx → pattern_mine_held) +
	// a traffic_mint_holds claim, both in-tx BEFORE the cap COUNT — so the over-cap
	// rollback still discards the held credit + hold row atomically.
	expectHeldRead(mock, "ws_e", 0, 0, 0)
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_e", micro(0.001), int64(0), TypePatternMineHeld, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_e", int64(0), micro(0.001), int64(0)). // balance unchanged, held 0→0.001, earned unchanged
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("INSERT INTO traffic_mint_holds").
		WithArgs("req-over", "ws_e", TypePatternMine, micro(0.001), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM routing_patterns").
		WithArgs("ws_e", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(3))) // 3 > cap 2
	mock.ExpectRollback()

	if err := miner.RecordPattern(context.Background(), "ws_e", earnPattern(), true, "req-over"); err != nil {
		t.Fatalf("over-cap must serve-but-skip (return nil), got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("over-cap must ROLLBACK (no commit), discarding row+credit: %v", err)
	}
}

// CAP-COUNT ERROR fail-closed: the cap COUNT errors → rollback, RecordPattern
// returns the error, no commit, no credit committed, no MintedTokens.
func TestRecordPattern_CapCountError_FailsClosed(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	miner.SetEarnCap(2, time.Hour)
	expectScoreRarity(mock, "ws_e", 0)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO pattern_mine_credits").
		WithArgs("req-cap", "ws_e", micro(0.001)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectQuery("INSERT INTO routing_patterns").
		WithArgs("ws_e", "code", "claude", "anthropic", InputBucketMedium,
			0.85, LatencyFast, 0.0, 1.0, 1, 0.0, "", true, micro(0.001)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow("p1", time.Now()))
	// Phase-4a Item 1: held credit + hold row, then the cap COUNT errors.
	expectHeldRead(mock, "ws_e", 0, 0, 0)
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_e", micro(0.001), int64(0), TypePatternMineHeld, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_e", int64(0), micro(0.001), int64(0)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("INSERT INTO traffic_mint_holds").
		WithArgs("req-cap", "ws_e", TypePatternMine, micro(0.001), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM routing_patterns").
		WithArgs("ws_e", pgxmock.AnyArg()).WillReturnError(errors.New("db down"))
	mock.ExpectRollback()

	if err := miner.RecordPattern(context.Background(), "ws_e", earnPattern(), true, "req-cap"); err == nil {
		t.Fatal("cap-count error must FAIL CLOSED (return error), not credit")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("cap-count error must ROLLBACK: %v", err)
	}
}

// REAL-PG -race EXACTNESS (the Pool-B 25-vs-5 proof): N concurrent same-workspace
// opted-in RecordPattern calls with cap=K → EXACTLY K credit (the cap COUNT rides
// CreditTx's lens_token_balances FOR UPDATE; the rest cap and roll back).
// earnTestPool spins up the real-PG schema the earn-path integration tests need
// (routing_patterns + the ledger tables + the S3 pattern_mine_credits claim
// table). Returns nil + skips when LENS_TEST_DATABASE_URL is unset.
func earnTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG earn-path test")
		return nil
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS routing_patterns`,
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`DROP TABLE IF EXISTS pattern_mine_credits`,
		`DROP TABLE IF EXISTS traffic_mint_holds`,
		// Phase-4a Item 1: pattern mint lands HELD in traffic_mint_holds.
		`CREATE TABLE traffic_mint_holds (request_id TEXT NOT NULL, workspace_id TEXT NOT NULL, mint_type TEXT NOT NULL,
			minted_amount BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (request_id, workspace_id, mint_type))`,
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
		// S3 claim table — composite UNIQUE(request_id, workspace_id).
		`CREATE TABLE pattern_mine_credits (id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			request_id TEXT NOT NULL, workspace_id TEXT NOT NULL, earned BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), UNIQUE (request_id, workspace_id))`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func TestRecordPattern_EarnCap_Exactness_Integration(t *testing.T) {
	pool := earnTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	miner := NewPatternMiner(newLedgerStore(pool), pool)
	const N, K = 25, 5
	miner.SetEarnCap(K, time.Hour)

	// DISTINCT request_ids per call so the CAP (not the claim) is what limits to
	// K — this test proves S2's cap exactness, not S3's claim dedup.
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		req := fmt.Sprintf("req-%d", i)
		wg.Add(1)
		go func(req string) {
			defer wg.Done()
			_ = miner.RecordPattern(ctx, "ws_race", earnPattern(), true, req)
		}(req)
	}
	wg.Wait()

	var earnedRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_patterns WHERE workspace_id='ws_race' AND earned > 0`).Scan(&earnedRows); err != nil {
		t.Fatal(err)
	}
	if earnedRows != K {
		t.Fatalf("EXACTNESS: %d concurrent earns vs cap %d → want exactly %d committed earn rows, got %d", N, K, K, earnedRows)
	}
	var bal int64
	if err := pool.QueryRow(ctx, `SELECT held_balance FROM lens_token_balances WHERE workspace_id='ws_race'`).Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if want := int64(K) * PatternBaseRate; bal != want { // integer µLENS — exact
		t.Fatalf("balance must equal exactly %d × base = %d µLENS; got %d", K, want, bal)
	}
}
