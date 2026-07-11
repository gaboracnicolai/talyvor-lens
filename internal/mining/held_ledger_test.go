package mining

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// expectHeldRead mocks the held kernel's two-step FOR-UPDATE read: ensure row
// (INSERT DO NOTHING) + SELECT balance, held_balance, lifetime_earned FOR UPDATE.
func expectHeldRead(mock pgxmock.PgxPoolIface, ws string, bal, held, earned int64) {
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs(ws).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, held_balance, lifetime_earned").
		WithArgs(ws).
		WillReturnRows(pgxmock.NewRows([]string{"balance", "held_balance", "lifetime_earned"}).AddRow(bal, held, earned))
}

// CREDIT-HELD (the 2.3a mint): held += amount; spendable balance AND
// lifetime_earned UNCHANGED (earnings are recognized at finalize, not mint).
// Ledger row: +amount, balance_after = spendable (unchanged), type
// pool_royalty_held — NOT in the supply allow-list, so a held mint does not
// count toward supply.
func TestCreditHeldTx_CreditsHeldOnly(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectHeldRead(mock, "wsA", micro(10.0), micro(2.0), micro(50.0))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("wsA", micro(3.0), micro(10.0), TypePoolRoyaltyHeld, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("wsA", micro(10.0), micro(5.0), micro(50.0)). // balance unchanged, held 2+3=5, earned unchanged
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	tx, err := mock.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreditHeldTx(context.Background(), tx, "wsA", micro(3.0), TypePoolRoyaltyHeld, "held mint", nil); err != nil {
		t.Fatalf("CreditHeldTx: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// FINALIZE (held -> spendable): conserves total owned value (bal+held), moves
// amount across, recognizes lifetime_earned NOW. Ledger row: +amount with the
// REAL new spendable balance_after, type pool_royalty — the EXISTING counted
// type, so supply counts at finalize.
func TestFinalizeHeldTx_MovesHeldToSpendable(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectHeldRead(mock, "wsA", micro(10.0), micro(5.0), micro(50.0))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("wsA", micro(5.0), micro(15.0), TypePoolRoyalty, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("wsA", micro(15.0), micro(0.0), micro(55.0)). // bal 10+5, held 5-5, earned 50+5; 10+5 == 15+0 conserved
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	tx, _ := mock.Begin(context.Background())
	if err := store.FinalizeHeldTx(context.Background(), tx, "wsA", micro(5.0), "finalize", nil); err != nil {
		t.Fatalf("FinalizeHeldTx: %v", err)
	}
	_ = tx.Commit(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestFinalizeHeldTx_InsufficientHeld(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectHeldRead(mock, "wsA", micro(10.0), micro(2.0), micro(50.0))
	mock.ExpectRollback()
	tx, _ := mock.Begin(context.Background())
	if err := store.FinalizeHeldTx(context.Background(), tx, "wsA", micro(5.0), "finalize", nil); !errors.Is(err, ErrInsufficientHeld) {
		t.Fatalf("want ErrInsufficientHeld, got %v", err)
	}
	_ = tx.Rollback(context.Background())
}

// REVOKE (burn-from-held, confirmed-bad mint): held -= amount; spendable AND
// earned untouched. Ledger row: -amount, balance_after = spendable (unchanged),
// type pool_royalty_revoked — NOT in the supply list AND NOT in the burned
// list (held LENS never entered supply, so revoking must not decrease
// circulating supply).
func TestRevokeHeldTx_BurnsFromHeldOnly(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectHeldRead(mock, "wsA", micro(10.0), micro(5.0), micro(50.0))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs("wsA", -micro(3.0), micro(10.0), TypePoolRoyaltyRevoked, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs("wsA", micro(10.0), micro(2.0), micro(50.0)). // balance unchanged, held 5-3=2, earned unchanged
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	tx, _ := mock.Begin(context.Background())
	if err := store.RevokeHeldTx(context.Background(), tx, "wsA", micro(3.0), "revoked: poisoning confirmed", nil); err != nil {
		t.Fatalf("RevokeHeldTx: %v", err)
	}
	_ = tx.Commit(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestRevokeHeldTx_InsufficientHeld(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	expectHeldRead(mock, "wsA", micro(10.0), micro(1.0), micro(50.0))
	mock.ExpectRollback()
	tx, _ := mock.Begin(context.Background())
	if err := store.RevokeHeldTx(context.Background(), tx, "wsA", micro(3.0), "revoke", nil); !errors.Is(err, ErrInsufficientHeld) {
		t.Fatalf("want ErrInsufficientHeld, got %v", err)
	}
	_ = tx.Rollback(context.Background())
}

func TestHeldOps_RejectNonPositive(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	tx, _ := mock.Begin(context.Background())
	for _, fn := range []func() error{
		func() error {
			return store.CreditHeldTx(context.Background(), tx, "ws", 0, TypePoolRoyaltyHeld, "", nil)
		},
		func() error { return store.FinalizeHeldTx(context.Background(), tx, "ws", -1, "", nil) },
		func() error { return store.RevokeHeldTx(context.Background(), tx, "ws", 0, "", nil) },
	} {
		if err := fn(); err == nil {
			t.Error("non-positive amount must error")
		}
	}
}
