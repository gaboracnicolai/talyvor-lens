package mining

import (
	"context"
	"errors"
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
	expectScoreRarity(mock, "ws_e", 0) // n=0 → rarity 0.0 → earned base 0.001
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO routing_patterns").
		WithArgs("ws_e", "code", "claude", "anthropic", InputBucketMedium,
			0.85, LatencyFast, 0.0, 1.0, 1, 0.0, true, 0.001).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow("p1", time.Now()))
	expectApplyTx(mock, "ws_e", 0, 0, 0, 0.001, 0.001, 0.001, 0)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM routing_patterns").
		WithArgs("ws_e", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(3))) // 3 > cap 2
	mock.ExpectRollback()

	if err := miner.RecordPattern(context.Background(), "ws_e", earnPattern(), true); err != nil {
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
	mock.ExpectQuery("INSERT INTO routing_patterns").
		WithArgs("ws_e", "code", "claude", "anthropic", InputBucketMedium,
			0.85, LatencyFast, 0.0, 1.0, 1, 0.0, true, 0.001).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow("p1", time.Now()))
	expectApplyTx(mock, "ws_e", 0, 0, 0, 0.001, 0.001, 0.001, 0)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM routing_patterns").
		WithArgs("ws_e", pgxmock.AnyArg()).WillReturnError(errors.New("db down"))
	mock.ExpectRollback()

	if err := miner.RecordPattern(context.Background(), "ws_e", earnPattern(), true); err == nil {
		t.Fatal("cap-count error must FAIL CLOSED (return error), not credit")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("cap-count error must ROLLBACK: %v", err)
	}
}

// REAL-PG -race EXACTNESS (the Pool-B 25-vs-5 proof): N concurrent same-workspace
// opted-in RecordPattern calls with cap=K → EXACTLY K credit (the cap COUNT rides
// CreditTx's lens_token_balances FOR UPDATE; the rest cap and roll back).
func TestRecordPattern_EarnCap_Exactness_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG earn-cap exactness test")
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
		`CREATE TABLE routing_patterns (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			feature_category TEXT NOT NULL, model_used TEXT NOT NULL, provider_used TEXT NOT NULL,
			input_token_range TEXT NOT NULL, output_quality DOUBLE PRECISION NOT NULL DEFAULT 0,
			latency_bucket TEXT NOT NULL, cache_hit_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
			success_rate DOUBLE PRECISION NOT NULL DEFAULT 1, sample_count INT NOT NULL DEFAULT 1,
			rarity DOUBLE PRECISION NOT NULL DEFAULT 0, opted_in BOOLEAN NOT NULL DEFAULT FALSE,
			earned DOUBLE PRECISION NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_earned DOUBLE PRECISION NOT NULL DEFAULT 0, lifetime_spent DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount DOUBLE PRECISION NOT NULL, balance_after DOUBLE PRECISION NOT NULL, type TEXT NOT NULL,
			description TEXT, metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	miner := NewPatternMiner(newLedgerStore(pool), pool)
	const N, K = 25, 5
	miner.SetEarnCap(K, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = miner.RecordPattern(ctx, "ws_race", earnPattern(), true)
		}()
	}
	wg.Wait()

	var earnedRows int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_patterns WHERE workspace_id='ws_race' AND earned > 0`).Scan(&earnedRows); err != nil {
		t.Fatal(err)
	}
	if earnedRows != K {
		t.Fatalf("EXACTNESS: %d concurrent earns vs cap %d → want exactly %d committed earn rows, got %d", N, K, K, earnedRows)
	}
	var bal float64
	if err := pool.QueryRow(ctx, `SELECT balance FROM lens_token_balances WHERE workspace_id='ws_race'`).Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if d := bal - float64(K)*PatternBaseRate; d < -1e-9 || d > 1e-9 {
		t.Fatalf("balance must equal exactly %d × base (%v); got %v", K, float64(K)*PatternBaseRate, bal)
	}
}
