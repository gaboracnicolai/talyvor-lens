package poolroyalty

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeLedger records CreditTx calls; it satisfies the minimal ledger seam the
// Minter needs (the real *mining.LedgerStore matches the same signature).
type creditCall struct {
	workspaceID string
	amount      float64
	txType      string
}

type fakeLedger struct {
	calls []creditCall
	err   error
}

func (f *fakeLedger) CreditTx(_ context.Context, _ pgx.Tx, workspaceID string, amount float64, txType, _ string, _ map[string]interface{}) error {
	f.calls = append(f.calls, creditCall{workspaceID: workspaceID, amount: amount, txType: txType})
	return f.err
}

func sampleHit() ServedHit {
	return ServedHit{
		RequestID:            "req-1",
		RequesterWorkspace:   "wsB",
		ContributorWorkspace: "wsA",
		Layer:                "exact",
		EntryID:              "lens:exact:deadbeef",
		Provider:             "openai",
		Model:                "gpt-4o",
		AvoidedCOGSUSD:       2.0,
	}
}

func enabledOn() bool  { return true }
func enabledOff() bool { return false }

// EXACTLY-ONCE: the first mint for a request_id claims the row and credits the
// contributor in one transaction; a second attempt with the same request_id
// hits the UNIQUE conflict (RowsAffected 0), performs NO ledger credit, and
// reports AlreadyMinted — the povi_challenges claim/RowsAffected guard.
func TestMintServedHit_ExactlyOncePerRequestID(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	// First serve: claim row inserted → credit → commit.
	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs("req-1", "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, 1.0).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectCommit()

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatalf("first MintServedHit: %v", err)
	}
	if !res.Minted || res.AlreadyMinted {
		t.Errorf("first serve: Minted=%v AlreadyMinted=%v, want true/false", res.Minted, res.AlreadyMinted)
	}
	if res.Amount != 1.0 { // 0.5 × 2.0
		t.Errorf("minted amount = %v, want 1.0 (s × avoided_COGS)", res.Amount)
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("ledger credits = %d, want 1", len(ledger.calls))
	}
	if c := ledger.calls[0]; c.workspaceID != "wsA" || c.amount != 1.0 || c.txType != TypePoolRoyalty {
		t.Errorf("credit = %+v, want wsA / 1.0 / %s", c, TypePoolRoyalty)
	}

	// Retry with the SAME request_id: UNIQUE conflict → no credit, no commit of
	// any ledger write. The claim insert wrote nothing, so the tx just ends.
	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs("req-1", "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, 1.0).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	pool.ExpectRollback()

	res2, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatalf("retry MintServedHit: %v", err)
	}
	if res2.Minted || !res2.AlreadyMinted {
		t.Errorf("retry: Minted=%v AlreadyMinted=%v, want false/true", res2.Minted, res2.AlreadyMinted)
	}
	if len(ledger.calls) != 1 {
		t.Errorf("ledger credits after retry = %d, want still 1 (exactly once)", len(ledger.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// DEFLATIONARY FAILURE DIRECTION: a reused request_id — even from a DIFFERENT
// hit (different contributor/entry) — suppresses the later mint. Collisions
// can only under-mint, never inflate supply.
func TestMintServedHit_ReusedRequestID_SuppressesNeverInflates(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	hit := sampleHit()
	hit.ContributorWorkspace = "wsC" // different contributor, same request_id
	hit.Layer = "semantic"
	hit.EntryID = "emb-row-9"
	hit.Similarity = 0.97

	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs("req-1", "wsB", "wsC", "semantic", "emb-row-9", "openai", "gpt-4o", 0.97, 2.0, 1.0).
		WillReturnResult(pgxmock.NewResult("INSERT", 0)) // claim already taken
	pool.ExpectRollback()

	res, err := m.MintServedHit(context.Background(), hit)
	if err != nil {
		t.Fatalf("MintServedHit: %v", err)
	}
	if res.Minted || !res.AlreadyMinted {
		t.Errorf("Minted=%v AlreadyMinted=%v, want false/true", res.Minted, res.AlreadyMinted)
	}
	if len(ledger.calls) != 0 {
		t.Errorf("ledger credits = %d, want 0 (suppressed)", len(ledger.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// SINGLE TRANSACTION: a CreditTx failure rolls the claim back with it — no
// orphan claim row (which would permanently suppress the contributor's mint),
// no orphan credit.
func TestMintServedHit_CreditFailureRollsBackClaim(t *testing.T) {
	pool := newMockPool(t)
	ledger := &fakeLedger{err: errors.New("ledger down")}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	pool.ExpectBegin()
	pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
		WithArgs("req-1", "wsB", "wsA", "exact", "lens:exact:deadbeef", "openai", "gpt-4o", 0.0, 2.0, 1.0).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectRollback() // claim + credit roll back together

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err == nil {
		t.Fatal("want error when CreditTx fails")
	}
	if res.Minted {
		t.Error("Minted must be false on rollback")
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations (claim must roll back, no commit): %v", err)
	}
}

// INERT BY DEFAULT: minting disabled → zero DB interaction, zero credits.
func TestMintServedHit_DisabledIsInert(t *testing.T) {
	pool := newMockPool(t) // NO expectations: any DB call fails the test
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOff)

	res, err := m.MintServedHit(context.Background(), sampleHit())
	if err != nil {
		t.Fatalf("disabled MintServedHit: %v", err)
	}
	if res.Minted || res.AlreadyMinted || len(ledger.calls) != 0 {
		t.Errorf("disabled minter must be a no-op; res=%+v credits=%d", res, len(ledger.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// CROSS-TENANT ONLY: a workspace served by its own pooled entry earns no
// royalty (there is no counterparty).
func TestMintServedHit_SelfHitDoesNotMint(t *testing.T) {
	pool := newMockPool(t) // NO expectations
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	hit := sampleHit()
	hit.ContributorWorkspace = hit.RequesterWorkspace
	res, err := m.MintServedHit(context.Background(), hit)
	if err != nil || res.Minted || len(ledger.calls) != 0 {
		t.Errorf("self-hit must not mint; res=%+v err=%v credits=%d", res, err, len(ledger.calls))
	}
}

// Defensive no-ops: empty request_id (no idempotency key → no mint), empty
// contributor (pre-feature entry), zero avoided_COGS, nil minter.
func TestMintServedHit_DefensiveNoOps(t *testing.T) {
	pool := newMockPool(t) // NO expectations
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, 0.5, enabledOn)

	noKey := sampleHit()
	noKey.RequestID = ""
	if res, err := m.MintServedHit(context.Background(), noKey); err != nil || res.Minted {
		t.Errorf("empty request_id must not mint; res=%+v err=%v", res, err)
	}

	noOwner := sampleHit()
	noOwner.ContributorWorkspace = ""
	if res, err := m.MintServedHit(context.Background(), noOwner); err != nil || res.Minted {
		t.Errorf("empty contributor must not mint; res=%+v err=%v", res, err)
	}

	free := sampleHit()
	free.AvoidedCOGSUSD = 0
	if res, err := m.MintServedHit(context.Background(), free); err != nil || res.Minted {
		t.Errorf("zero avoided_COGS must not mint; res=%+v err=%v", res, err)
	}

	var nilM *Minter
	if res, err := nilM.MintServedHit(context.Background(), sampleHit()); err != nil || res.Minted {
		t.Errorf("nil minter must be a safe no-op; res=%+v err=%v", res, err)
	}

	if len(ledger.calls) != 0 {
		t.Errorf("no defensive case may credit; credits=%d", len(ledger.calls))
	}
}

// BURN-AND-MINT INVARIANT: minted = s × avoided_COGS and Talyvor's net
// (1−s) × avoided_COGS ≥ 0 for every share in [0,1]; out-of-range shares are
// clamped at construction so the invariant cannot be violated by config.
func TestRoyaltyShare_InvariantAndClamping(t *testing.T) {
	for _, tc := range []struct {
		in, want float64
	}{
		{0.0, 0.0}, {0.3, 0.3}, {0.5, 0.5}, {1.0, 1.0},
		{-0.5, 0.0}, // clamped low
		{1.5, 1.0},  // clamped high
	} {
		m := NewMinter(nil, nil, tc.in, enabledOn)
		if got := m.Share(); got != tc.want {
			t.Errorf("NewMinter(share=%v).Share() = %v, want %v", tc.in, got, tc.want)
		}
		const cogs = 3.0
		minted := m.Share() * cogs
		talyvorNet := (1 - m.Share()) * cogs
		if minted < 0 || talyvorNet < 0 {
			t.Errorf("share=%v: minted=%v talyvorNet=%v — invariant (1−s)×COGS ≥ 0 violated", tc.in, minted, talyvorNet)
		}
		if math.Abs(minted+talyvorNet-cogs) > 1e-9 {
			t.Errorf("share=%v: minted+net=%v, want %v (conservation)", tc.in, minted+talyvorNet, cogs)
		}
	}
}
