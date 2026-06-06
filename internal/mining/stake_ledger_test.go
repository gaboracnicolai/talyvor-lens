package mining

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// expectStakeOp mocks the two-step FOR-UPDATE balance read used by the
// lock/release/slash txs: INSERT DO NOTHING (ensure row) + SELECT FOR UPDATE.
func expectStakeRead(mock pgxmock.PgxPoolIface, ws string, bal, locked float64) {
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs(ws).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, locked_balance").
		WithArgs(ws).
		WillReturnRows(pgxmock.NewRows([]string{"balance", "locked_balance"}).AddRow(bal, locked))
}

// Locking stake moves LENS available→locked atomically; the balances row
// reflects the split (balance down, locked up); total owned LENS is conserved.
func TestLockStake_MovesAvailableToLocked(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectStakeRead(mock, "ws", 10.0, 2.0)
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws", -5.0, 5.0, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws", 5.0, 7.0). // balance 10-5=5, locked 2+5=7
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := store.LockStake(context.Background(), "ws", 5.0, nil); err != nil {
		t.Fatalf("LockStake: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// Can't lock more than the available balance (collateral must be real).
func TestLockStake_InsufficientBalance(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectStakeRead(mock, "ws", 3.0, 0.0)
	mock.ExpectRollback()
	if err := store.LockStake(context.Background(), "ws", 5.0, nil); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
}

// Releasing moves locked→available (the operator gets their collateral back).
func TestReleaseStake_MovesLockedToAvailable(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectStakeRead(mock, "ws", 5.0, 7.0)
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws", 7.0, 12.0, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws", 12.0, 0.0). // balance 5+7=12, locked 7-7=0
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := store.ReleaseStake(context.Background(), "ws", 7.0, nil); err != nil {
		t.Fatalf("ReleaseStake: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestReleaseStake_InsufficientLocked(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectStakeRead(mock, "ws", 5.0, 2.0)
	mock.ExpectRollback()
	if err := store.ReleaseStake(context.Background(), "ws", 5.0, nil); !errors.Is(err, ErrInsufficientLocked) {
		t.Fatalf("want ErrInsufficientLocked, got %v", err)
	}
}

// Slashing BURNS locked LENS: it leaves locked and does NOT return to available
// (supply is reduced). balance unchanged, locked down.
func TestSlashStake_BurnsLocked(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectStakeRead(mock, "ws", 5.0, 7.0)
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("ws", -3.0, 5.0, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()). // balance_after unchanged = 5
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("ws", 5.0, 4.0). // balance unchanged 5, locked 7-3=4
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := store.SlashStake(context.Background(), "ws", 3.0, nil); err != nil {
		t.Fatalf("SlashStake: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestSlashStake_InsufficientLocked(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectStakeRead(mock, "ws", 5.0, 2.0)
	mock.ExpectRollback()
	if err := store.SlashStake(context.Background(), "ws", 5.0, nil); !errors.Is(err, ErrInsufficientLocked) {
		t.Fatalf("want ErrInsufficientLocked, got %v", err)
	}
}

// THE PART-2 BLOCKER FIX (required before minting is safe): a slash burns
// LENS, so circulating supply must DECREASE by the slashed amount. The burned
// query must count povi_stake_slash rows, not just plain burns.
func TestGetCirculatingSupply_CountsSlashBurns(t *testing.T) {
	store, mock := newMockStore(t)
	// total minted (incl. pool_royalty since Stage 2.2)
	mock.ExpectQuery(`amount > 0 AND type IN`).
		WithArgs(TypeCacheMine, TypeComputeMine, TypeEmbeddingMine, TypeAnnotationMine, TypePatternMine, TypePoolRoyalty).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(1000.0))
	// burned: MUST include both plain burns AND stake slashes.
	mock.ExpectQuery(`SUM\(-amount\)`).
		WithArgs(TypeBurn, TypeStakeSlash).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(30.0)) // 10 burn + 20 slash

	got, err := store.GetCirculatingSupply(context.Background())
	if err != nil {
		t.Fatalf("GetCirculatingSupply: %v", err)
	}
	if got != 970.0 {
		t.Errorf("circulating = %v, want 970 (1000 minted − 30 burned incl. slash)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (burned query must count slash burns): %v", err)
	}
}

// GetTotalBurned must also count slash burns, so the economy-stats display
// (GetEconomyStats = total − GetTotalBurned) stays consistent with the
// slash-aware GetCirculatingSupply.
func TestGetTotalBurned_CountsSlashBurns(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectQuery(`SUM\(-amount\)`).
		WithArgs(TypeBurn, TypeStakeSlash).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(42.0))
	got, err := store.GetTotalBurned(context.Background())
	if err != nil {
		t.Fatalf("GetTotalBurned: %v", err)
	}
	if got != 42.0 {
		t.Errorf("burned = %v, want 42 (burn + slash)", got)
	}
}

func TestStakeOps_RejectNonPositive(t *testing.T) {
	store, _ := newMockStore(t)
	for _, fn := range []func() error{
		func() error { return store.LockStake(context.Background(), "ws", 0, nil) },
		func() error { return store.ReleaseStake(context.Background(), "ws", -1, nil) },
		func() error { return store.SlashStake(context.Background(), "ws", 0, nil) },
	} {
		if err := fn(); err == nil {
			t.Error("non-positive amount must error")
		}
	}
}
