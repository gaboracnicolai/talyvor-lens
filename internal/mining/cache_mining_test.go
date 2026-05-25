package mining

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// expectCreditOrDebit programmes the mock with one full Begin →
// upsert balance → INSERT ledger → UPDATE balance → Commit cycle.
// startingBalance is what the SELECT for update returns.
func expectCreditOrDebit(
	mock pgxmock.PgxPoolIface,
	workspaceID string,
	startingBalance, startingEarned, startingSpent float64,
	delta, expectedBalance, expectedEarned, expectedSpent float64,
) {
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO lens_token_balances").
		WithArgs(workspaceID).
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(startingBalance, startingEarned, startingSpent))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs(workspaceID, delta, expectedBalance, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs(workspaceID, expectedBalance, expectedEarned, expectedSpent).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
}

func newMockStore(t *testing.T) (*LedgerStore, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return newLedgerStore(mock), mock
}

// ─── Credit / Debit ──────────────────────────────

func TestCredit_IncreasesBalance(t *testing.T) {
	store, mock := newMockStore(t)
	// Starting at 0, credit 0.010 → balance 0.010, lifetime_earned 0.010.
	expectCreditOrDebit(mock, "ws_c", 0, 0, 0, 0.010, 0.010, 0.010, 0)
	err := store.Credit(context.Background(), "ws_c", 0.010, TypeCacheMine, "", nil)
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestDebit_DecreasesBalance(t *testing.T) {
	store, mock := newMockStore(t)
	// Starting at 1.0, debit 0.25 → balance 0.75, lifetime_spent 0.25.
	expectCreditOrDebit(mock, "ws_d", 1.0, 1.0, 0, -0.25, 0.75, 1.0, 0.25)
	err := store.Debit(context.Background(), "ws_d", 0.25, TypeSpend, "", nil)
	if err != nil {
		t.Fatalf("Debit: %v", err)
	}
}

func TestDebit_InsufficientBalance(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO lens_token_balances").
		WithArgs("ws_e").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(0.05, 0.05, 0.0))
	mock.ExpectRollback()
	err := store.Debit(context.Background(), "ws_e", 0.50, TypeSpend, "", nil)
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestCredit_RejectsZeroOrNegative(t *testing.T) {
	store, _ := newMockStore(t)
	if err := store.Credit(context.Background(), "ws", 0, TypeCacheMine, "", nil); err == nil {
		t.Fatal("expected error for zero credit")
	}
	if err := store.Credit(context.Background(), "ws", -1, TypeCacheMine, "", nil); err == nil {
		t.Fatal("expected error for negative credit")
	}
}

// ─── GetBalance / GetSnapshot ────────────────────

func TestGetBalance_ReturnsZeroForNew(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery("SELECT balance FROM lens_token_balances").
		WithArgs("ws_new").
		WillReturnError(errNoRows())
	b, err := store.GetBalance(context.Background(), "ws_new")
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if b != 0 {
		t.Fatalf("expected 0.0 for new workspace, got %f", b)
	}
}

func TestGetSnapshot_NewWorkspaceReturnsZeroSnapshot(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery("SELECT workspace_id, balance, lifetime_earned").
		WithArgs("ws_snap").
		WillReturnError(errNoRows())
	snap, err := store.GetSnapshot(context.Background(), "ws_snap")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.WorkspaceID != "ws_snap" || snap.Balance != 0 {
		t.Fatalf("expected zero snapshot, got %+v", snap)
	}
}

// ─── GetHistory ──────────────────────────────────

func TestGetHistory_PaginatedResults(t *testing.T) {
	store, mock := newMockStore(t)
	now := time.Now().UTC()
	mock.ExpectQuery("SELECT id, workspace_id, amount, balance_after").
		WithArgs("ws_h", 10, 20).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "amount", "balance_after",
			"type", "description", "metadata", "created_at",
		}).
			AddRow("e1", "ws_h", 0.010, 0.010, TypeCacheMine, "hit", []byte(`{"hit_type":"exact"}`), now).
			AddRow("e2", "ws_h", 0.001, 0.011, TypeCacheMine, "hit", []byte(`{}`), now))
	entries, err := store.GetHistory(context.Background(), "ws_h", 10, 20)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != "e1" || entries[0].Amount != 0.010 {
		t.Fatalf("unexpected entry[0]: %+v", entries[0])
	}
	if entries[0].Metadata["hit_type"] != "exact" {
		t.Fatalf("metadata not unmarshalled, got %+v", entries[0].Metadata)
	}
}

// ─── CacheMiner rate selection ───────────────────

func TestRecordCacheHit_SameWorkspaceTinyReward(t *testing.T) {
	store, mock := newMockStore(t)
	miner := NewCacheMiner(store, true)
	// Same workspace → CacheHitSameWorkspace = 0.001.
	expectCreditOrDebit(mock, "ws_a", 0, 0, 0, 0.001, 0.001, 0.001, 0)
	if err := miner.RecordCacheHit(context.Background(), "ws_a", "ws_a", "exact"); err != nil {
		t.Fatalf("RecordCacheHit: %v", err)
	}
}

func TestRecordCacheHit_CrossWorkspaceBigReward(t *testing.T) {
	store, mock := newMockStore(t)
	miner := NewCacheMiner(store, true)
	expectCreditOrDebit(mock, "ws_owner", 0, 0, 0, 0.010, 0.010, 0.010, 0)
	if err := miner.RecordCacheHit(context.Background(), "ws_owner", "ws_other", "exact"); err != nil {
		t.Fatalf("RecordCacheHit: %v", err)
	}
}

func TestRecordCacheHit_SemanticHit(t *testing.T) {
	store, mock := newMockStore(t)
	miner := NewCacheMiner(store, true)
	expectCreditOrDebit(mock, "ws_owner", 0, 0, 0, 0.005, 0.005, 0.005, 0)
	if err := miner.RecordCacheHit(context.Background(), "ws_owner", "ws_other", "semantic"); err != nil {
		t.Fatalf("RecordCacheHit: %v", err)
	}
}

func TestRecordCacheHit_SharingDisabledFallsBackToSame(t *testing.T) {
	store, mock := newMockStore(t)
	miner := NewCacheMiner(store, false) // sharing off
	// Even though requester differs, sharing-off means tiny reward.
	expectCreditOrDebit(mock, "ws_owner", 0, 0, 0, 0.001, 0.001, 0.001, 0)
	if err := miner.RecordCacheHit(context.Background(), "ws_owner", "ws_other", "exact"); err != nil {
		t.Fatalf("RecordCacheHit: %v", err)
	}
}

func TestRecordCacheHit_EmptyOwnerNoOp(t *testing.T) {
	store, _ := newMockStore(t)
	miner := NewCacheMiner(store, true)
	// No expectations: store must NOT be touched.
	if err := miner.RecordCacheHit(context.Background(), "", "ws_req", "exact"); err != nil {
		t.Fatalf("RecordCacheHit: %v", err)
	}
}

// ─── GetMiningStats ──────────────────────────────

func TestGetMiningStats_ReadsSnapshot(t *testing.T) {
	store, mock := newMockStore(t)
	miner := NewCacheMiner(store, true)
	// Pretend the miner has logged some in-memory counters first.
	_ = miner.RecordCacheHit(context.Background(), "", "anyone", "exact") // no-op, just to exercise the path
	miner.served["ws_stats"] = 7
	miner.benefited["ws_stats"] = 3
	mock.ExpectQuery("SELECT workspace_id, balance, lifetime_earned").
		WithArgs("ws_stats").
		WillReturnRows(pgxmock.NewRows([]string{
			"workspace_id", "balance", "lifetime_earned", "lifetime_spent", "updated_at",
		}).AddRow("ws_stats", 1.23, 2.50, 1.27, time.Now()))
	stats, err := miner.GetMiningStats(context.Background(), "ws_stats")
	if err != nil {
		t.Fatalf("GetMiningStats: %v", err)
	}
	if stats.CurrentBalance != 1.23 || stats.LifetimeEarned != 2.50 {
		t.Fatalf("unexpected totals: %+v", stats)
	}
	if stats.CacheHitsServed != 7 || stats.CacheHitsBenefited != 3 {
		t.Fatalf("unexpected counters: %+v", stats)
	}
}

// ─── Rates() ─────────────────────────────────────

func TestRates_ReturnsExpectedKeys(t *testing.T) {
	r := Rates()
	for _, want := range []string{"cache_hit_same", "cache_hit_cross", "semantic_hit"} {
		if _, ok := r[want]; !ok {
			t.Fatalf("missing rate key %q", want)
		}
	}
	if r["cache_hit_same"] != CacheHitSameWorkspace {
		t.Fatalf("rate value drift: %v", r["cache_hit_same"])
	}
}

// errNoRows is the pgx.ErrNoRows sentinel surfaced through
// pgxmock — saves importing pgx directly in the test file.
func errNoRows() error { return errPgxNoRows }
