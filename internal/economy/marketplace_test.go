package economy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/mining"
)

// SEC-2: LENS amounts are integer µLENS (uLENS = 1e6, declared in dualtoken_test.go).
// Prices (price_usd) and APY are Tier-2/3 floats and stay float64.

// expectCreditInTx programmes the mock for one CreditTx call inside a
// caller-owned transaction — the 4 SQL operations without Begin/Commit.
// Balances are integer µLENS.
func expectCreditInTx(
	mock pgxmock.PgxPoolIface,
	workspaceID string,
	startingBal, startingEarned, startingSpent int64,
	delta, expectedBal, expectedEarned, expectedSpent int64,
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
	// Transfer 2.5 LENS. Lock order: "ws_from" < "ws_to" → ws_from locked first.
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_from").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_from").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(10*uLENS, 10*uLENS, int64(0)))
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_to").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_to").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(int64(0), int64(0), int64(0)))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_from", -25*uLENS/10, 75*uLENS/10, mining.TypeTransferOut, "test", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_from", 75*uLENS/10, 10*uLENS, 25*uLENS/10).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_to", 25*uLENS/10, 25*uLENS/10, mining.TypeTransferIn, "test", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_to", 25*uLENS/10, 25*uLENS/10, int64(0)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := ledger.Transfer(context.Background(), "ws_from", "ws_to", 25*uLENS/10, "test"); err != nil {
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
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_broke").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_broke").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(uLENS/10, uLENS/10, int64(0)))
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_other").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_other").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(int64(0), int64(0), int64(0)))
	mock.ExpectRollback()
	err = ledger.Transfer(context.Background(), "ws_broke", "ws_other", 5*uLENS, "test")
	if !errors.Is(err, mining.ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestTransfer_RejectsSelfTransfer(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	ledger := mining.NewLedgerStoreForTesting(mock)
	if err := ledger.Transfer(context.Background(), "ws_a", "ws_a", 1*uLENS, "x"); err == nil {
		t.Fatal("expected error for self-transfer")
	}
}

func TestTransfer_RejectsBelowMinimum(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	ledger := mining.NewLedgerStoreForTesting(mock)
	// 0.0001 LENS = 100 µLENS, below MinTransferAmount (1_000 µLENS).
	if err := ledger.Transfer(context.Background(), "a", "b", 100, "x"); err == nil {
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
			AddRow(5*uLENS, 5*uLENS, int64(0)))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_burn", -1*uLENS, 4*uLENS, mining.TypeBurn, "ai-spend", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_burn", 4*uLENS, 5*uLENS, 1*uLENS).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	if err := ledger.Burn(context.Background(), "ws_burn", 1*uLENS, "ai-spend"); err != nil {
		t.Fatalf("Burn: %v", err)
	}
}

func TestGetCirculatingSupply_TotalMinusBurned(t *testing.T) {
	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	ledger := mining.NewLedgerStoreForTesting(mock)
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(amount\\), 0\\)\\s+FROM lens_token_ledger\\s+WHERE amount > 0").
		WithArgs(mining.CountedSupplyTypes()). // one array arg: type = ANY($1)
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(100 * uLENS))
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(-amount\\), 0\\) FROM lens_token_ledger WHERE type").
		WithArgs(mining.TypeBurn, mining.TypeStakeSlash).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(15 * uLENS))
	got, err := ledger.GetCirculatingSupply(context.Background())
	if err != nil {
		t.Fatalf("GetCirculatingSupply: %v", err)
	}
	if got != 85*uLENS {
		t.Fatalf("expected %d µLENS, got %d", 85*uLENS, got)
	}
}

// ─── Marketplace: listings ───────────────────────

func TestCreateListing_ValidatesSellerBalance(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_poor").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_poor").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(uLENS/2, uLENS/2, int64(0)))
	mock.ExpectRollback()
	_, err := store.CreateListing(context.Background(), MarketplaceListing{
		SellerID: "ws_poor", Amount: 50 * uLENS, PriceUSD: 0.10,
	})
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestCreateListing_RejectsBelowMinimum(t *testing.T) {
	store, _ := newStore(t)
	// 0.5 LENS = 500_000 µLENS, below MinListingAmount (1_000_000 µLENS).
	_, err := store.CreateListing(context.Background(), MarketplaceListing{
		SellerID: "ws", Amount: uLENS / 2, PriceUSD: 0.10,
	})
	if err == nil {
		t.Fatal("expected error for sub-minimum listing")
	}
}

func TestCancelListing_OnlyAllowsSeller(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list1", "ws_seller", 50*uLENS, 0.10, 0.0,
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
	// 5% fee on 10 = 0.5 LENS to talyvor. Net to buyer = 9.5. Unsold = 40 LENS.
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list1", "ws_seller", 50*uLENS, 0.10, 0.0,
			ListingActive, nil, time.Now()))
	expectCreditInTx(mock, "ws_buyer", 0, 0, 0, 95*uLENS/10, 95*uLENS/10, 95*uLENS/10, 0)
	expectCreditInTx(mock, TalyvorWorkspace, 0, 0, 0, uLENS/2, uLENS/2, uLENS/2, 0)
	expectCreditInTx(mock, "ws_seller", 0, 0, 0, 40*uLENS, 40*uLENS, 40*uLENS, 0)
	mock.ExpectExec("UPDATE marketplace_listings SET status").
		WithArgs(ListingFilled, "list1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("INSERT INTO marketplace_trades").
		WithArgs("list1", "ws_buyer", "ws_seller", 10*uLENS, 0.10, uLENS/2).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("trade1", time.Now()))
	mock.ExpectCommit()

	trade, err := store.ExecuteTrade(context.Background(), "list1", "ws_buyer", 1.0)
	if err != nil {
		t.Fatalf("ExecuteTrade: %v", err)
	}
	if trade.Amount != 10*uLENS || trade.TalyvorFee != uLENS/2 {
		t.Fatalf("unexpected trade: %+v", trade)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestExecuteTrade_RejectsBuyOwnListing(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list_self").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list_self", "ws_self", 10*uLENS, 0.10, 0.0,
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

func TestExecuteTrade_RejectsFilledListing(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list_filled").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list_filled", "ws_seller", 10*uLENS, 0.10, 0.0,
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

func TestExecuteTrade_LostRaceGuard(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list_race").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list_race", "ws_seller", 50*uLENS, 0.10, 0.0,
			ListingActive, nil, time.Now()))
	expectCreditInTx(mock, "ws_buyer", 0, 0, 0, 95*uLENS/10, 95*uLENS/10, 95*uLENS/10, 0)
	expectCreditInTx(mock, TalyvorWorkspace, 0, 0, 0, uLENS/2, uLENS/2, uLENS/2, 0)
	expectCreditInTx(mock, "ws_seller", 0, 0, 0, 40*uLENS, 40*uLENS, 40*uLENS, 0)
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

func TestCancelListing_Success(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list_cancel").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list_cancel", "ws_seller", 50*uLENS, 0.10, 0.0,
			ListingActive, nil, time.Now()))
	mock.ExpectExec("UPDATE marketplace_listings SET status").
		WithArgs(ListingCancelled, "list_cancel").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	expectCreditInTx(mock, "ws_seller", 0, 0, 0, 50*uLENS, 50*uLENS, 50*uLENS, 0)
	mock.ExpectCommit()
	if err := store.CancelListing(context.Background(), "list_cancel", "ws_seller"); err != nil {
		t.Fatalf("CancelListing: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCancelListing_LostRace(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, seller_id, amount, price_usd").
		WithArgs("list_race_cancel").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "seller_id", "amount", "price_usd", "min_buy_usd",
			"status", "filled_at", "created_at",
		}).AddRow("list_race_cancel", "ws_seller", 50*uLENS, 0.10, 0.0,
			ListingActive, nil, time.Now()))
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
	_, err := store.Stake(context.Background(), "ws", 10*uLENS, 60)
	if !errors.Is(err, ErrInvalidLockDays) {
		t.Fatalf("expected ErrInvalidLockDays, got %v", err)
	}
}

func TestStake_90DayUsesCorrectAPY(t *testing.T) {
	store, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_s").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_s").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(100*uLENS, 100*uLENS, int64(0)))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_s", -50*uLENS, 50*uLENS, "stake", "LENS staked for yield", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_s", 50*uLENS, 100*uLENS, 50*uLENS).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("INSERT INTO stake_positions").
		WithArgs("ws_s", 50*uLENS, 90, APY90, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "started_at"}).
			AddRow("stake1", time.Now()))
	mock.ExpectCommit()
	pos, err := store.Stake(context.Background(), "ws_s", 50*uLENS, 90)
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
	// Unstake now reads the row FOR UPDATE inside the tx, so the tx opens before
	// the read and rolls back on the still-locked early return.
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, workspace_id, amount, lock_days").
		WithArgs("stake1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "amount", "lock_days", "apy", "started_at", "unlocks_at",
		}).AddRow("stake1", "ws_s", 50*uLENS, 30, APY30,
			time.Now().Add(-time.Hour), future))
	mock.ExpectRollback()
	err := store.Unstake(context.Background(), "stake1", "ws_s")
	if !errors.Is(err, ErrStakeLocked) {
		t.Fatalf("expected ErrStakeLocked, got %v", err)
	}
}

func TestUnstake_AfterLockCreditsYield(t *testing.T) {
	store, mock := newStore(t)
	started := time.Now().Add(-31 * 24 * time.Hour)
	unlocks := time.Now().Add(-time.Hour)
	// Unstake now opens the tx first, reads the row FOR UPDATE inside it, then
	// deletes (RowsAffected must be 1 to credit) and credits — all one tx.
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, workspace_id, amount, lock_days").
		WithArgs("stake_done").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "amount", "lock_days", "apy", "started_at", "unlocks_at",
		}).AddRow("stake_done", "ws_u", 100*uLENS, 30, APY30, started, unlocks))
	mock.ExpectExec("DELETE FROM stake_positions").
		WithArgs("stake_done").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs("ws_u").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs("ws_u").
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(int64(0), int64(0), int64(0)))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws_u", pgxmock.AnyArg(), pgxmock.AnyArg(), "unstake", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws_u", pgxmock.AnyArg(), pgxmock.AnyArg(), int64(0)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	if err := store.Unstake(context.Background(), "stake_done", "ws_u"); err != nil {
		t.Fatalf("Unstake: %v", err)
	}
}

// ─── computeYield maths ─────────────────────────

func TestComputeYield_Math(t *testing.T) {
	// 100 LENS at 20% APY for 365 days → exactly 20 LENS yield (integer µLENS).
	got := computeYield(100*uLENS, 0.20, 365*24*time.Hour)
	if got != 20*uLENS {
		t.Fatalf("expected exactly %d µLENS yield over a year, got %d", 20*uLENS, got)
	}
	// 100 LENS at 5% APY for 30 days → ~0.411 LENS ≈ 411_000 µLENS.
	got = computeYield(100*uLENS, 0.05, 30*24*time.Hour)
	if got < 400_000 || got > 420_000 {
		t.Fatalf("expected ~0.41 LENS (≈411_000 µLENS) yield, got %d", got)
	}
}
