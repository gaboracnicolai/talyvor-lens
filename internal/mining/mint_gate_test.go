package mining

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

type fakeVerifier struct {
	ok  bool
	err error
}

func (f fakeVerifier) MayEarn(context.Context, pgx.Tx, string) (bool, error) { return f.ok, f.err }

// TestMintTypes_GateSet pins the gate's mint-type SET and the DOCUMENTED reason
// it differs from GetTotalSupply's allow-list. A future "consistency fix"
// aligning the two would silently un-gate the held mint or double-gate finalize
// — a Sybil hole introduced by tidying. This test is the tripwire for that.
func TestMintTypes_GateSet(t *testing.T) {
	// The seven mint-MOMENT types must be gated.
	for _, ty := range []string{
		TypeCacheMine, TypeComputeMine, TypeEmbeddingMine, TypeAnnotationMine,
		TypePatternMine, "receipt_mine_provisional", TypePoolRoyaltyHeld,
	} {
		if !IsMintType(ty) {
			t.Errorf("%q must be a gated mint type", ty)
		}
	}
	// pool_royalty_held (the held MINT — the worst Sybil hole) MUST be gated even
	// though supply counts it LATER as pool_royalty. Dropping it "to match
	// supply" would un-gate it.
	if !IsMintType(TypePoolRoyaltyHeld) {
		t.Error("pool_royalty_held (held mint moment) must be gated")
	}
	// The finalize SETTLEMENT (pool_royalty) and the burn (pool_royalty_revoked)
	// are NOT mints — gating finalize would double-gate already-gated value.
	for _, ty := range []string{TypePoolRoyalty, TypePoolRoyaltyRevoked} {
		if IsMintType(ty) {
			t.Errorf("%q is a settlement/burn, not a mint moment — must NOT be gated", ty)
		}
	}
	// Conservation moves and spends are never gated.
	for _, ty := range []string{"marketplace_buy", "marketplace_fee", "unstake", "annotation_unstake", TypeTransfer, TypeSpend} {
		if IsMintType(ty) {
			t.Errorf("conservation type %q must NOT be gated", ty)
		}
	}
}

// TestCreditOnce_EmptyRequestID_FailsClosed — no server-derived key ⇒ no mint
// (returns before any DB work; the mock sees no SQL).
func TestCreditOnce_EmptyRequestID_FailsClosed(t *testing.T) {
	store, _ := newMockStore(t)
	if _, err := store.CreditOnce(context.Background(), "", "ws", 1.0, TypeComputeMine, "", nil); !errors.Is(err, ErrNoMintRequestID) {
		t.Fatalf("empty requestID must fail closed with ErrNoMintRequestID, got %v", err)
	}
}

// TestVerifiedGate_BlocksUnverified — a mint-type credit by an unverified
// workspace is blocked at the chokepoint: the claim is written then the verify
// gate fails before any balance SQL, and the whole tx rolls back (ErrEarnNotVerified).
func TestVerifiedGate_BlocksUnverified(t *testing.T) {
	store, mock := newMockStore(t)
	store.SetMintVerifier(fakeVerifier{ok: false})
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO mint_idempotency").
		WithArgs("r1", "ws", TypeComputeMine, pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectRollback() // verifyEarn blocks before the ensure-balance INSERT
	if _, err := store.CreditOnce(context.Background(), "r1", "ws", 1.0, TypeComputeMine, "", nil); !errors.Is(err, ErrEarnNotVerified) {
		t.Fatalf("unverified mint must be blocked with ErrEarnNotVerified, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestVerifiedGate_AllowsVerified — a verified workspace mints normally (full
// claim + credit cycle).
func TestVerifiedGate_AllowsVerified(t *testing.T) {
	store, mock := newMockStore(t)
	store.SetMintVerifier(fakeVerifier{ok: true})
	expectCreditOnce(mock, "r1", "ws", TypeComputeMine, 0, 0, 0, 1.0, 1.0, 1.0, 0)
	if already, err := store.CreditOnce(context.Background(), "r1", "ws", 1.0, TypeComputeMine, "", nil); err != nil || already {
		t.Fatalf("verified mint must succeed: already=%v err=%v", already, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestVerifiedGate_ConservationUngated — a conservation credit (unstake) by an
// unverified workspace is NOT gated: verifyEarn is a no-op for non-mint types,
// so the credit proceeds. Proves the gate discriminates mint from move.
func TestVerifiedGate_ConservationUngated(t *testing.T) {
	store, mock := newMockStore(t)
	store.SetMintVerifier(fakeVerifier{ok: false}) // would block a mint, but...
	// ...unstake is conservation → not a mint type → no verify, full credit.
	expectCreditOrDebit(mock, "ws", 0, 0, 0, 5.0, 5.0, 5.0, 0)
	if err := store.Credit(context.Background(), "ws", 5.0, "unstake", "", nil); err != nil {
		t.Fatalf("conservation credit must NOT be gated, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestCreditOnce_SecondClaimSuppressed — replaying the same
// (requestID, workspace, mint_type) does NOT double-mint: the claim INSERT
// returns 0 rows and the credit is skipped (alreadyMinted=true).
func TestCreditOnce_SecondClaimSuppressed(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO mint_idempotency").
		WithArgs("r1", "ws", TypeComputeMine, pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 0)) // already claimed
	mock.ExpectRollback()
	already, err := store.CreditOnce(context.Background(), "r1", "ws", 1.0, TypeComputeMine, "", nil)
	if err != nil || !already {
		t.Fatalf("replay must suppress the mint (alreadyMinted): already=%v err=%v", already, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
