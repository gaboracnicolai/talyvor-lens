package economy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/mining"
)

// expectCreditTx programmes the mock for one full LedgerStore.Credit
// transaction: Begin → INSERT DO NOTHING (ensure row) → SELECT FOR UPDATE →
// INSERT ledger row → UPDATE balance → Commit.
// Used by standalone Credit calls (e.g. Stake, CancelListing, Unstake).
func expectCreditTx(
	mock pgxmock.PgxPoolIface,
	workspaceID string,
	startingBal, startingEarned, startingSpent float64,
	delta, expectedBal, expectedEarned, expectedSpent float64,
) {
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs(workspaceID).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs(workspaceID).
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(startingBal, startingEarned, startingSpent))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs(workspaceID, delta, expectedBal, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs(workspaceID, expectedBal, expectedEarned, expectedSpent).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
}

// expectCreditInTx programmes the mock for one CreditTx call inside a
// caller-owned transaction — the 4 SQL operations without Begin/Commit.
// Used when multiple credits share a single transaction (e.g. ExecuteTrade).
func expectCreditInTx(
	mock pgxmock.PgxPoolIface,
	workspaceID string,
	startingBal, startingEarned, startingSpent float64,
	delta, expectedBal, expectedEarned, expectedSpent float64,
) {
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs(workspaceID).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs(workspaceID).
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(startingBal, startingEarned, startingSpent))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs(workspaceID, delta, expectedBal, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs(workspaceID, expectedBal, expectedEarned, expectedSpent).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
}

func newStore(t *testing.T) (*MarketplaceStore, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	ledger := mining.NewLedgerStoreForTesting(mock)
	return newMarketplaceStore(ledger, mock), mock
}

// ─── Transfer / Burn on LedgerStore ──────────────

func TestTransfer_MovesTokens(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ledger := mining.NewLedgerStoreForTesting(mock)
	// Transfer atomically debits + credits inside one tx.
	// Lock order: "ws_from" < "ws_to" lexicographically → ws_from locked first.
	// Both locks are acquired BEFORE any writes (deadlock-prevention fix).
	mock.ExpectBegin()
	// Lock 1: ws_from (lex-smaller) — ensure row + FOR UPDATE read.
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_from").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_from").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(10.0, 10.0, 0.0))
	// Lock 2: ws_to — ensure row + FOR UPDATE read (before any writes).
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_to").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_to").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(0.0, 0.0, 0.0))
	// Writes follow: debit ws_from, credit ws_to.
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_from", -2.5, 7.5, mining.TypeTransferOut, "test", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_from", 7.5, 10.0, 2.5).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_to", 2.5, 2.5, mining.TypeTransferIn, "test", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_to", 2.5, 2.5, 0.0).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := ledger.Transfer(context.Background(), "ws_from", "ws_to", 2.5, "test"); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
}

func TestTransfer_InsufficientBalance(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ledger := mining.NewLedgerStoreForTesting(mock)
	// "ws_broke" < "ws_other" lexicographically → ws_broke locked first.
	// Both locks must be acquired before the balance check; expect both reads.
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_broke").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_broke").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(0.1, 0.1, 0.0))
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_other").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_other").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(0.0, 0.0, 0.0))
	mock.ExpectRollback()
	err = ledger.Transfer(context.Background(), "ws_broke", "ws_other", 5.0, "test")
	if !errors.Is(err, mining.ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestTransfer_RejectsSelfTransfer(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	ledger := mining.NewLedgerStoreForTesting(mock)
	if err := ledger.Transfer(context.Background(), "ws_a", "ws_a", 1.0, "x"); err == nil {
		t.Fatal("expected error for self-transfer")
	}
}

func TestTransfer_RejectsBelowMinimum(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	ledger := mining.NewLedgerStoreForTesting(mock)
	if err := ledger.Transfer(context.Background(), "a", "b", 0.0001, "x"); err == nil {
		t.Fatal("expected error for sub-minimum transfer")
	}
}

func TestBurn_ReducesBalance(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()
	ledger := mining.NewLedgerStoreForTesting(mock)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_burn").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_burn").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(5.0, 5.0, 0.0))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_burn", -1.0, 4.0, mining.TypeBurn, "ai-spend", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_burn", 4.0, 5.0, 1.0).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	if err := ledger.Burn(context.Background(), "ws_burn", 1.0, "ai-spend"); err != nil {
		t.Fatalf("Burn: %v", err)
	}
}

func TestGetCirculatingSupply_TotalMinusBurned(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	ledger := mining.NewLedgerStoreForTesting(mock)
	// GetTotalSupply
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(amount\\), 0\\)\\s+FROM lens_token_ledger\\s+WHERE amount > 0").
		WithArgs(mining.TypeCacheMine, mining.TypeComputeMine, mining.TypeEmbeddingMine,
			mining.TypeAnnotationMine, mining.TypePatternMine, mining.TypePoolRoyalty).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(100.0))
	// Burned
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(-amount\\), 0\\) FROM lens_token_ledger WHERE type").
		WithArgs(mining.TypeBurn, mining.TypeStakeSlash).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(15.0))
	got, err := ledger.GetCirculatingSupply(context.Background())
	if err != nil {
		t.Fatalf("GetCirculatingSupply: %v", err)
	}
	if got != 85.0 {
		t.Fatalf("expected 85.0, got %f", got)
	}
}

// ─── Marketplace: listings ───────────────────────

func TestCreateListing_ValidatesSellerBalance(t *testing.T) {
	store, mock := newStore(t)
	// Debit will fail (ErrInsufficientBalance via apply).
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_poor").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_poor").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(0.5, 0.5, 0.0))
	mock.ExpectRollback()
	_, err := store.CreateListing(context.Background(), MarketplaceListing{
		SellerID: "ws_poor", Amount: 50, PriceUSD: 0.10,
	})
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestCreateListing_RejectsBelowMinimum(t *testing.T) {
	store, _ := newStore(t)
	_, err := store.CreateListing(context.Background(), MarketplaceListing{
		SellerID: "ws", Amount: 0.5, PriceUSD: 0.10,
	})
	if err == nil {
		t.Fatal("expected error for sub-minimum listing")
	}
}

func TestCancelListing_OnlyAllowsSeller(t *testing.T) {
	store, mock := newStore(t)
	// SELECT FOR UPDATE is now inside the transaction.
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list1", "ws_seller", 50.0, 0.10, 0.0,
			ListingActive, nil, time.Now()))
	mock.ExpectRollback()
	err := store.CancelListing(context.Background(), "list1", "ws_imposter")
	if !errors.Is(err, ErrNotSeller) {
		t.Fatalf("expected ErrNotSeller, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ─── Marketplace: ExecuteTrade ───────────────────

func TestExecuteTrade_TransfersCorrectAmounts(t *testing.T) {
	store, mock := newStore(t)
	// Listing: 50 LENS @ $0.10 each, buyer pays $1.00 → 10 LENS.
	// 5% fee on 10 = 0.5 LENS to talyvor. Net to buyer = 9.5.
	// Unsold = 40 LENS → refund seller.
	//
	// SELECT FOR UPDATE + all five mutations (3 credits + UPDATE + INSERT)
	// share one transaction so a failure at any step rolls back atomically.
	mock.ExpectBegin()
	// Read + lock the listing row inside the transaction (FOR UPDATE).
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list1", "ws_seller", 50.0, 0.10, 0.0,
			ListingActive, nil, time.Now()))
	// CreditTx buyer 9.5 LENS (no Begin/Commit — inside shared tx).
	expectCreditInTx(mock, "ws_buyer", 0, 0, 0, 9.5, 9.5, 9.5, 0)
	// CreditTx talyvor 0.5 LENS fee.
	expectCreditInTx(mock, TalyvorWorkspace, 0, 0, 0, 0.5, 0.5, 0.5, 0)
	// CreditTx seller 40 LENS unsold refund.
	expectCreditInTx(mock, "ws_seller", 0, 0, 0, 40.0, 40.0, 40.0, 0)
	// Mark listing filled — UPDATE now carries AND status='active' guard.
	mock.ExpectExec("UPDATE marketplace_listings SET status").
		WithArgs(ListingFilled, "list1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// Insert trade row (within same tx).
	mock.ExpectQuery("INSERT INTO marketplace_trades").
		WithArgs("list1", "ws_buyer", "ws_seller", 10.0, 0.10, 0.5).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("trade1", time.Now()))
	mock.ExpectCommit()

	trade, err := store.ExecuteTrade(context.Background(), "list1", "ws_buyer", 1.0)
	if err != nil {
		t.Fatalf("ExecuteTrade: %v", err)
	}
	if trade.Amount != 10.0 || trade.TalyvorFee != 0.5 {
		t.Fatalf("unexpected trade: %+v", trade)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestExecuteTrade_RejectsBuyOwnListing(t *testing.T) {
	store, mock := newStore(t)
	// SELECT FOR UPDATE is now inside the transaction; error before commit
	// triggers the deferred rollback.
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list_self").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list_self", "ws_self", 10.0, 0.10, 0.0,
			ListingActive, nil, time.Now()))
	mock.ExpectRollback()
	_, err := store.ExecuteTrade(context.Background(), "list_self", "ws_self", 1.0)
	if err == nil {
		t.Fatal("expected error for buying own listing")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteTrade_RejectsFilledListing covers the primary race path:
// the SELECT FOR UPDATE sees a listing that is already 'filled' (another
// buyer won the lock first) — no credits are issued, no trade is recorded.
func TestExecuteTrade_RejectsFilledListing(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list_filled").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list_filled", "ws_seller", 10.0, 0.10, 0.0,
			ListingFilled, nil, time.Now()))
	mock.ExpectRollback()
	_, err := store.ExecuteTrade(context.Background(), "list_filled", "ws_buyer", 1.0)
	if !errors.Is(err, ErrListingNotActive) {
		t.Fatalf("expected ErrListingNotActive for already-filled listing, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestExecuteTrade_LostRaceGuard covers the defence-in-depth path: the
// SELECT FOR UPDATE saw 'active', credits were issued inside the transaction,
// but the AND status='active' guard on the UPDATE returns 0 rows — the
// whole transaction rolls back and no LENS is transferred to anyone.
func TestExecuteTrade_LostRaceGuard(t *testing.T) {
	store, mock := newStore(t)
	// 50 LENS @ $0.10, buyer pays $1.00 → 10 LENS; unsold 40.
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list_race").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list_race", "ws_seller", 50.0, 0.10, 0.0,
			ListingActive, nil, time.Now()))
	expectCreditInTx(mock, "ws_buyer", 0, 0, 0, 9.5, 9.5, 9.5, 0)
	expectCreditInTx(mock, TalyvorWorkspace, 0, 0, 0, 0.5, 0.5, 0.5, 0)
	expectCreditInTx(mock, "ws_seller", 0, 0, 0, 40.0, 40.0, 40.0, 0)
	// Race lost: UPDATE returns 0 rows → rollback (credits never commit).
	mock.ExpectExec("UPDATE marketplace_listings SET status").
		WithArgs(ListingFilled, "list_race").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()
	_, err := store.ExecuteTrade(context.Background(), "list_race", "ws_buyer", 1.0)
	if !errors.Is(err, ErrListingNotActive) {
		t.Fatalf("expected ErrListingNotActive for lost-race trade, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestCancelListing_Success is the happy-path cancel: seller cancels an
// active listing, gets the full amount refunded.
func TestCancelListing_Success(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list_cancel").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list_cancel", "ws_seller", 50.0, 0.10, 0.0,
			ListingActive, nil, time.Now()))
	mock.ExpectExec("UPDATE marketplace_listings SET status").
		WithArgs(ListingCancelled, "list_cancel").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// Refund 50 LENS to seller.
	expectCreditInTx(mock, "ws_seller", 0, 0, 0, 50.0, 50.0, 50.0, 0)
	mock.ExpectCommit()
	if err := store.CancelListing(context.Background(), "list_cancel", "ws_seller"); err != nil {
		t.Fatalf("CancelListing: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestCancelListing_LostRace covers the double-refund prevention path:
// a concurrent ExecuteTrade fills the listing between the SELECT FOR UPDATE
// and the cancel's UPDATE, so the AND status='active' guard returns 0 rows.
// The cancel must return ErrListingNotActive and not credit any refund.
func TestCancelListing_LostRace(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list_race_cancel").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list_race_cancel", "ws_seller", 50.0, 0.10, 0.0,
			ListingActive, nil, time.Now()))
	// Race lost: UPDATE returns 0 rows — no refund issued.
	mock.ExpectExec("UPDATE marketplace_listings SET status").
		WithArgs(ListingCancelled, "list_race_cancel").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()
	err := store.CancelListing(context.Background(), "list_race_cancel", "ws_seller")
	if !errors.Is(err, ErrListingNotActive) {
		t.Fatalf("expected ErrListingNotActive for lost-race cancel, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ─── Staking ─────────────────────────────────────

func TestStake_RejectsInvalidLockDays(t *testing.T) {
	store, _ := newStore(t)
	_, err := store.Stake(context.Background(), "ws", 10, 60)
	if !errors.Is(err, ErrInvalidLockDays) {
		t.Fatalf("expected ErrInvalidLockDays, got %v", err)
	}
}

func TestStake_90DayUsesCorrectAPY(t *testing.T) {
	store, mock := newStore(t)
	// Debit stake from balance.
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_s").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_s").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(100.0, 100.0, 0.0))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_s", -50.0, 50.0, "stake", "LENS staked for yield", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_s", 50.0, 100.0, 50.0).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// INSERT stake position (inside same tx as the Debit — atomicity fix).
	mock.ExpectQuery("INSERT INTO stake_positions").
		WithArgs("ws_s", 50.0, 90, APY90, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "started_at"}).
			AddRow("stake1", time.Now()))
	mock.ExpectCommit()
	pos, err := store.Stake(context.Background(), "ws_s", 50.0, 90)
	if err != nil {
		t.Fatalf("Stake: %v", err)
	}
	if pos.APY != APY90 {
		t.Fatalf("expected APY %f, got %f", APY90, pos.APY)
	}
}

func TestUnstake_BeforeLockFails(t *testing.T) {
	store, mock := newStore(t)
	future := time.Now().Add(7 * 24 * time.Hour)
	mock.ExpectQuery("SELECT id, workspace_id, amount, lock_days").
		WithArgs("stake1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "amount", "lock_days", "apy", "started_at", "unlocks_at",
		}).AddRow("stake1", "ws_s", 50.0, 30, APY30,
			time.Now().Add(-time.Hour), future))
	err := store.Unstake(context.Background(), "stake1", "ws_s")
	if !errors.Is(err, ErrStakeLocked) {
		t.Fatalf("expected ErrStakeLocked, got %v", err)
	}
}

func TestUnstake_AfterLockCreditsYield(t *testing.T) {
	store, mock := newStore(t)
	// Position created 31 days ago, 30-day lock → unlocked.
	// 30 days × (5% APY / 365) × 100 LENS ≈ 0.41 yield.
	started := time.Now().Add(-31 * 24 * time.Hour)
	unlocks := time.Now().Add(-time.Hour)
	mock.ExpectQuery("SELECT id, workspace_id, amount, lock_days").
		WithArgs("stake_done").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "amount", "lock_days", "apy", "started_at", "unlocks_at",
		}).AddRow("stake_done", "ws_u", 100.0, 30, APY30, started, unlocks))
	// DELETE + Credit run inside one transaction (atomicity fix).
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM stake_positions").
		WithArgs("stake_done").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	// Credit principal + yield inside same tx (CreditTx — no own Begin/Commit).
	// We don't pin the exact yield figure because it depends on wall clock.
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_u").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_u").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(0.0, 0.0, 0.0))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_u", pgxmock.AnyArg(), pgxmock.AnyArg(), "unstake", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_u", pgxmock.AnyArg(), pgxmock.AnyArg(), 0.0).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	if err := store.Unstake(context.Background(), "stake_done", "ws_u"); err != nil {
		t.Fatalf("Unstake: %v", err)
	}
}

// ─── computeYield maths ─────────────────────────

func TestComputeYield_Math(t *testing.T) {
	// 100 LENS at 20% APY for 365 days → 20 LENS yield.
	got := computeYield(100, 0.20, 365*24*time.Hour)
	diff := got - 20.0
	if diff < -0.01 || diff > 0.01 {
		t.Fatalf("expected 20 LENS yield over a year, got %f", got)
	}
	// 100 LENS at 5% APY for 30 days → ~0.411 yield.
	got = computeYield(100, 0.05, 30*24*time.Hour)
	if got < 0.40 || got > 0.42 {
		t.Fatalf("expected ~0.41 yield, got %f", got)
	}
}
