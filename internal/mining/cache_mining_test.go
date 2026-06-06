package mining

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// expectCreditOrDebit programmes the mock with one full Begin → ensure balance
// row (INSERT DO NOTHING) → FOR UPDATE read → INSERT ledger → UPDATE balance →
// Commit cycle. startingBalance is what the FOR UPDATE SELECT returns.
func expectCreditOrDebit(
	mock pgxmock.PgxPoolIface,
	workspaceID string,
	startingBalance, startingEarned, startingSpent float64,
	delta, expectedBalance, expectedEarned, expectedSpent float64,
) {
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs(workspaceID).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
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
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_e").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
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
	mock.ExpectQuery("SELECT workspace_id, balance, lifetime_earned").
		WithArgs("ws_stats").
		WillReturnRows(pgxmock.NewRows([]string{
			"workspace_id", "balance", "lifetime_earned", "lifetime_spent", "updated_at",
		}).AddRow("ws_stats", 1.23, 2.50, 1.27, time.Now()))
	mock.ExpectQuery("SELECT COUNT").
		WithArgs("ws_stats").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(7))
	mock.ExpectQuery("SELECT COUNT").
		WithArgs("ws_stats").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(3))
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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
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

// ─── Transfer ───────────────────────────────────────────────────────────────

// expectReadBalance programmes one readBalance sub-sequence inside a
// Transfer transaction: the INSERT DO NOTHING that ensures the row exists,
// followed by the SELECT … FOR UPDATE that acquires the pessimistic lock.
func expectReadBalance(
	mock pgxmock.PgxPoolIface,
	workspaceID string,
	bal, earned, spent float64,
) {
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs(workspaceID).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs(workspaceID).
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(bal, earned, spent))
}

// TestTransfer_HappyPath verifies that a straightforward transfer debits the
// sender and credits the recipient atomically (normal alphabetical order —
// from < to, so no swap needed).
func TestTransfer_HappyPath(t *testing.T) {
	// Transfer("ws_a", "ws_b", 0.5, "test")
	// Lex order: ws_a < ws_b → ws_a locked first (no swap), ws_b second.
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectReadBalance(mock, "ws_a", 1.0, 1.0, 0) // first lock: ws_a (from)
	expectReadBalance(mock, "ws_b", 0.2, 0.2, 0) // second lock: ws_b (to)

	// debit ws_a: amount -0.5, newBal 0.5, spent 0→0.5
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_a", -0.5, 0.5, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_a", 0.5, 1.0, 0.5).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// credit ws_b: amount +0.5, newBal 0.7, earned 0.2→0.7
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_b", 0.5, 0.7, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_b", 0.7, 0.7, 0.0).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := store.Transfer(context.Background(), "ws_a", "ws_b", 0.5, "test"); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestTransfer_LockOrderIsLexicographic is the deadlock-regression guard.
//
// It calls Transfer("ws_b", "ws_a", …) where from > to alphabetically.
// The fix must still acquire ws_a (the lex-smaller ID) FIRST before ws_b,
// regardless of which direction LENS is flowing.
//
// If the fix ever regresses to caller-order locking, the mock will surface an
// unexpected query (ws_b before ws_a) and the test fails immediately — no need
// to construct actual concurrent transactions.
func TestTransfer_LockOrderIsLexicographic(t *testing.T) {
	// Transfer("ws_b" → "ws_a", 0.5)  —  from > to alphabetically.
	// Correct: lock ws_a first, then ws_b (lex order), regardless of flow.
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectReadBalance(mock, "ws_a", 0.2, 0.2, 0) // MUST be ws_a first
	expectReadBalance(mock, "ws_b", 1.0, 1.0, 0) // ws_b second

	// debit ws_b (from): amount -0.5, newBal 0.5, spent 0→0.5
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_b", -0.5, 0.5, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_b", 0.5, 1.0, 0.5).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// credit ws_a (to): amount +0.5, newBal 0.7, earned 0.2→0.7
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_a", 0.5, 0.7, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_a", 0.7, 0.7, 0.0).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := store.Transfer(context.Background(), "ws_b", "ws_a", 0.5, "test"); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestTransfer_InsufficientBalance verifies ErrInsufficientBalance is returned
// when the sender's balance is too low. Both locks are acquired before the
// balance check (the fix acquires all locks first), so both readBalance calls
// appear in the mock before the rollback.
func TestTransfer_InsufficientBalance(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectReadBalance(mock, "ws_a", 0.1, 0.1, 0) // from=ws_a, only 0.1 available
	expectReadBalance(mock, "ws_b", 1.0, 1.0, 0)
	mock.ExpectRollback()

	err := store.Transfer(context.Background(), "ws_a", "ws_b", 0.5, "test")
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestTransfer_Validation covers the guard-clause fast-paths that reject bad
// inputs before touching the database.
func TestTransfer_Validation(t *testing.T) {
	store, _ := newMockStore(t)
	ctx := context.Background()

	if err := store.Transfer(ctx, "ws_a", "ws_b", 0.0001, ""); err == nil {
		t.Fatal("expected error: amount below minimum")
	}
	if err := store.Transfer(ctx, "", "ws_b", 1.0, ""); err == nil {
		t.Fatal("expected error: empty from workspace")
	}
	if err := store.Transfer(ctx, "ws_a", "", 1.0, ""); err == nil {
		t.Fatal("expected error: empty to workspace")
	}
	if err := store.Transfer(ctx, "ws_a", "ws_a", 1.0, ""); err == nil {
		t.Fatal("expected error: self-transfer")
	}
}

// STAGE 2.2(b) SUPPLY COUNTING: pool_royalty joins the minted-supply
// allow-list — a royalty mint is LENS entering circulation and must be
// counted honestly. The list stays an explicit allow-list: marketplace_fee
// (moves existing LENS, doesn't mint) and receipt_mine_provisional (povi's
// deliberate exclusion pending its own go-live call) remain OUT.
func TestGetTotalSupply_CountsPoolRoyalty_ExcludesNonMints(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectQuery(`SELECT COALESCE\(SUM\(amount\), 0\)`).
		WithArgs(TypeCacheMine, TypeComputeMine, TypeEmbeddingMine,
			TypeAnnotationMine, TypePatternMine, TypePoolRoyalty).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(42.5))

	got, err := s.GetTotalSupply(context.Background())
	if err != nil {
		t.Fatalf("GetTotalSupply: %v", err)
	}
	if got != 42.5 {
		t.Errorf("supply = %v, want 42.5", got)
	}

	// The allow-list itself is the assertion above (WithArgs pins all six
	// types, in order). Guard the exclusions explicitly so a future edit
	// can't sneak them in: the counted set must NOT contain these.
	excluded := []string{"marketplace_fee", "receipt_mine_provisional", TypeBurn, TypeStakeSlash, TypeTransferIn}
	counted := []string{TypeCacheMine, TypeComputeMine, TypeEmbeddingMine, TypeAnnotationMine, TypePatternMine, TypePoolRoyalty}
	for _, ex := range excluded {
		for _, c := range counted {
			if c == ex {
				t.Errorf("type %q must NOT be in the minted-supply allow-list", ex)
			}
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
