package mining

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// DUPLICATE SUPPRESSION (unit): same (request_id, workspace_id) — the claim
// conflicts (RowsAffected 0) → return nil, rollback, NO credit, NO routing row.
// (claim-first: a replay writes nothing — not even the INSERT.)
func TestRecordPattern_DuplicateRequest_Suppressed(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	miner.SetEarnCap(0, time.Hour)     // isolate the claim (cap disabled)
	expectScoreRarity(mock, "ws_e", 0) // n=0 → earned base 0.001
	mock.ExpectBegin()
	// claim conflicts → 0 rows affected → suppressed before any INSERT.
	mock.ExpectExec("INSERT INTO pattern_mine_credits").
		WithArgs("req-dup", "ws_e", micro(0.001)).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectRollback()

	if err := miner.RecordPattern(context.Background(), "ws_e", earnPattern(), true, "req-dup"); err != nil {
		t.Fatalf("duplicate must suppress (return nil), got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("duplicate claim must ROLLBACK with no INSERT/credit: %v", err)
	}
}

// EMPTY requestID → FAIL-CLOSED: persists as a capture-equivalent observation
// (earned=0, rarity=0), NO claim, NO credit — exactly the existing earned<=0 path.
func TestRecordPattern_EmptyRequestID_NoCreditPersistsObservation(t *testing.T) {
	miner, mock := newMockPatternMiner(t)
	miner.SetEarnCap(0, time.Hour)
	expectScoreRarity(mock, "ws_e", 0) // would earn 0.001, but empty requestID downgrades it
	mock.ExpectBegin()
	// NO claim ExpectExec. INSERT persists with earned=0, rarity=0, opted_in=true.
	mock.ExpectQuery("INSERT INTO routing_patterns").
		WithArgs("ws_e", "code", "claude", "anthropic", InputBucketMedium,
			0.85, LatencyFast, 0.0, 1.0, 1, 0.0 /*rarity*/, "" /*complexity_bucket*/, true /*opted_in*/, int64(0) /*earned*/).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).AddRow("p1", time.Now()))
	mock.ExpectCommit()

	if err := miner.RecordPattern(context.Background(), "ws_e", earnPattern(), true, "" /*no requestID*/); err != nil {
		t.Fatalf("empty requestID must not error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("empty requestID must persist earned=0 row, NO claim, NO credit: %v", err)
	}
}

// CROSS-WORKSPACE (real-PG, THE divergence proof): same request_id, DIFFERENT
// workspace_id → BOTH claim, BOTH credit. A bare UNIQUE(request_id) would let
// the first workspace suppress the second; the composite key does not collide.
func TestRecordPattern_CrossWorkspace_BothCredit_Integration(t *testing.T) {
	pool := earnTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	miner := NewPatternMiner(newLedgerStore(pool), pool)
	miner.SetEarnCap(0, time.Hour) // isolate the claim

	if err := miner.RecordPattern(ctx, "ws_A", earnPattern(), true, "shared-req"); err != nil {
		t.Fatal(err)
	}
	if err := miner.RecordPattern(ctx, "ws_B", earnPattern(), true, "shared-req"); err != nil {
		t.Fatal(err)
	}
	// Both workspaces have a claim row for the SAME request_id (composite scopes it).
	for _, ws := range []string{"ws_A", "ws_B"} {
		var claims int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pattern_mine_credits WHERE request_id='shared-req' AND workspace_id=$1`, ws).Scan(&claims); err != nil {
			t.Fatal(err)
		}
		if claims != 1 {
			t.Fatalf("workspace %s must hold its own claim for the shared request_id; got %d", ws, claims)
		}
		var bal float64
		if err := pool.QueryRow(ctx, `SELECT balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&bal); err != nil {
			t.Fatal(err)
		}
		if bal <= 0 {
			t.Fatalf("workspace %s must have CREDITED (composite key didn't collide); balance=%v", ws, bal)
		}
	}
}

// DUPLICATE (real-PG -race): concurrent same (request_id, workspace_id) → exactly
// one credits (the composite UNIQUE + ON CONFLICT serialize to one claim).
func TestRecordPattern_ConcurrentDuplicate_ExactlyOne_Integration(t *testing.T) {
	pool := earnTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	miner := NewPatternMiner(newLedgerStore(pool), pool)
	miner.SetEarnCap(0, time.Hour)

	const N = 20
	var ok int64
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := miner.RecordPattern(ctx, "ws_dup", earnPattern(), true, "same-req"); err == nil {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()

	var claims int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pattern_mine_credits WHERE request_id='same-req' AND workspace_id='ws_dup'`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 1 {
		t.Fatalf("concurrent duplicate must claim EXACTLY once; got %d", claims)
	}
	var bal int64
	if err := pool.QueryRow(ctx, `SELECT balance FROM lens_token_balances WHERE workspace_id='ws_dup'`).Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if bal != PatternBaseRate {
		t.Fatalf("exactly one credit of base (%d µLENS) must land; balance=%d", PatternBaseRate, bal)
	}
}

// CLAIM/CAP ROLLBACK ATOMICITY (real-PG, the dimension-3 invariant): an over-cap
// earn rolls back the CLAIM row TOO (single tx), so a capped request leaves NO
// orphan claim — a later retry (once there's cap room) can re-claim and re-earn.
// (Guards against ever splitting the claim out of the credit/cap tx.)
func TestRecordPattern_OverCap_RollsBackClaim_RetryReEarns_Integration(t *testing.T) {
	pool := earnTestPool(t)
	if pool == nil {
		return
	}
	ctx := context.Background()
	miner := NewPatternMiner(newLedgerStore(pool), pool)
	miner.SetEarnCap(1, time.Hour) // cap = 1

	// req-A: 1st earn, under cap → claims + credits.
	if err := miner.RecordPattern(ctx, "ws_c", earnPattern(), true, "req-A"); err != nil {
		t.Fatal(err)
	}
	// req-B (distinct request): claims, but the cap COUNT sees req-A + req-B = 2 > 1
	// → over-cap → return nil → the deferred rollback discards req-B's claim AND credit.
	if err := miner.RecordPattern(ctx, "ws_c", earnPattern(), true, "req-B"); err != nil {
		t.Fatal(err)
	}
	// req-B left NO orphan claim row (rolled back with the capped credit).
	var bClaims int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pattern_mine_credits WHERE request_id='req-B' AND workspace_id='ws_c'`).Scan(&bClaims); err != nil {
		t.Fatal(err)
	}
	if bClaims != 0 {
		t.Fatalf("a capped earn must leave NO orphan claim for req-B; got %d", bClaims)
	}
	var bal int64
	if err := pool.QueryRow(ctx, `SELECT balance FROM lens_token_balances WHERE workspace_id='ws_c'`).Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if bal != PatternBaseRate {
		t.Fatalf("only req-A credited (req-B capped+rolled-back); balance=%d want %d", bal, PatternBaseRate)
	}

	// Cap room opens (raise to 2) → req-B RETRIES: no orphan claim suppresses it,
	// so it re-claims and re-earns. This is the no-orphan-claim invariant.
	miner.SetEarnCap(2, time.Hour)
	if err := miner.RecordPattern(ctx, "ws_c", earnPattern(), true, "req-B"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT balance FROM lens_token_balances WHERE workspace_id='ws_c'`).Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if bal != 2*PatternBaseRate {
		t.Fatalf("req-B retry must re-earn (capped claim was rolled back, not orphaned); balance=%d want %d", bal, 2*PatternBaseRate)
	}
}
