package povi

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func staticLookup(pub ed25519.PublicKey) PubKeyLookup {
	return func(_ context.Context, _ string) (ed25519.PublicKey, error) { return pub, nil }
}

func alwaysEligible(_ context.Context, _ string) bool { return true }
func neverEligible(_ context.Context, _ string) bool  { return false }

// PART 2 GATE: even a verified receipt with minting ON must NOT mint when the
// node is not stake-eligible — the receipt is recorded but ineligible.
func TestProcess_StakeIneligible_NoMintEvenWhenVerifiedAndEnabled(t *testing.T) {
	pub, priv, _ := GenerateNodeKey()
	m := &fakeMinter{}
	p := NewProcessor(NewStore(nil), m, staticLookup(pub), neverEligible, true) // minting ON

	res, err := p.Process(context.Background(), SignReceipt(priv, sampleReceipt()))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Verified {
		t.Error("receipt should still verify")
	}
	if res.StakeEligible {
		t.Error("node should be reported ineligible")
	}
	if res.Minted || len(m.calls) != 0 {
		t.Error("an ineligible node must never mint, even verified + enabled")
	}
}

// A valid receipt with minting OFF (default): verified + recorded for audit,
// but NO mint.
func TestProcess_ValidReceipt_MintingOff_RecordsNoMint(t *testing.T) {
	pub, priv, _ := GenerateNodeKey()
	m := &fakeMinter{}
	p := NewProcessor(NewStore(nil), m, staticLookup(pub), alwaysEligible, false)

	res, err := p.Process(context.Background(), SignReceipt(priv, sampleReceipt()))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Verified {
		t.Error("valid receipt should verify")
	}
	if res.Minted {
		t.Error("minting is OFF by default — must not mint")
	}
	if len(m.calls) != 0 {
		t.Errorf("ledger credited %d times with minting off", len(m.calls))
	}
}

// A forged receipt (signature won't verify under the node's key) → unverified,
// not minted, recorded as unverified.
func TestProcess_ForgedReceipt_Unverified_NoMint(t *testing.T) {
	pub, priv, _ := GenerateNodeKey()
	m := &fakeMinter{}
	p := NewProcessor(NewStore(nil), m, staticLookup(pub), alwaysEligible, true) // even with minting ON

	r := SignReceipt(priv, sampleReceipt())
	r.OutputTokens = 999999 // tamper AFTER signing → signature invalid
	res, err := p.Process(context.Background(), r)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Verified {
		t.Error("tampered receipt must NOT verify")
	}
	if res.Minted || len(m.calls) != 0 {
		t.Error("an unverified receipt must never mint, even with minting enabled")
	}
}

// A node with no registered pubkey can't be verified → unverified, no mint.
func TestProcess_NoPubKey_Unverified(t *testing.T) {
	_, priv, _ := GenerateNodeKey()
	m := &fakeMinter{}
	lookup := func(_ context.Context, _ string) (ed25519.PublicKey, error) {
		return nil, errors.New("node has no pubkey on file")
	}
	p := NewProcessor(NewStore(nil), m, lookup, alwaysEligible, true)

	res, err := p.Process(context.Background(), SignReceipt(priv, sampleReceipt()))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Verified || res.Minted {
		t.Error("a node without a registered pubkey can't be verified or minted")
	}
}

// A valid receipt with minting explicitly ON (unsafe/test) → verified + a
// provisional mint to the node owner's workspace.
func TestProcess_ValidReceipt_MintingOn_ProvisionalMint(t *testing.T) {
	pub, priv, _ := GenerateNodeKey()
	m := &fakeMinter{}
	p := NewProcessor(NewStore(nil), m, staticLookup(pub), alwaysEligible, true)

	r := sampleReceipt()
	r.WorkspaceID = "ws-owner"
	r.OutputTokens = 1000
	res, err := p.Process(context.Background(), SignReceipt(priv, r))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Verified || !res.Minted || res.Amount <= 0 {
		t.Fatalf("expected verified+minted, got %+v", res)
	}
	if len(m.calls) != 1 || m.calls[0].workspaceID != "ws-owner" {
		t.Errorf("expected 1 credit to ws-owner, got %+v", m.calls)
	}
}

// LATENT-BUG REGRESSION (folded into Stage 2.1): a REPLAYED receipt — same
// request_id, so RecordReceipt's ON CONFLICT (request_id) DO NOTHING affects 0
// rows — must NOT mint again, even verified + stake-eligible + minting ON.
// Before the fix, Process ignored the conflict result and re-credited the
// ledger on every replay (double-mint). Same claim/RowsAffected guard as
// povi_challenges and pool_royalty_mints.
func TestProcess_ReplayedReceipt_MintsExactlyOnce(t *testing.T) {
	pub, priv, _ := GenerateNodeKey()
	m := &fakeMinter{}
	pool := newStorePool(t)

	r := sampleReceipt()
	r.WorkspaceID = "ws-owner"
	r.OutputTokens = 1000
	signed := SignReceipt(priv, r)
	rootHex := hex.EncodeToString(signed.MerkleRoot[:])

	// First arrival claims the request_id; the replay conflicts.
	pool.ExpectExec(`INSERT INTO povi_receipts`).
		WithArgs(signed.RequestID, signed.NodeID, signed.WorkspaceID, signed.Model,
			signed.InputTokens, signed.OutputTokens, rootHex, true, signed.Timestamp, signed.LeafCount, string(LeafKindRune)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectExec(`INSERT INTO povi_receipts`).
		WithArgs(signed.RequestID, signed.NodeID, signed.WorkspaceID, signed.Model,
			signed.InputTokens, signed.OutputTokens, rootHex, true, signed.Timestamp, signed.LeafCount, string(LeafKindRune)).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	p := NewProcessor(newStore(pool), m, staticLookup(pub), alwaysEligible, true)

	res1, err := p.Process(context.Background(), signed)
	if err != nil {
		t.Fatalf("first Process: %v", err)
	}
	if !res1.Verified || !res1.Minted || len(m.calls) != 1 {
		t.Fatalf("first arrival must mint once; res=%+v credits=%d", res1, len(m.calls))
	}

	res2, err := p.Process(context.Background(), signed)
	if err != nil {
		t.Fatalf("replay Process: %v", err)
	}
	if res2.Minted {
		t.Error("replayed receipt must NOT mint (request_id already recorded)")
	}
	if len(m.calls) != 1 {
		t.Errorf("ledger credits = %d, want exactly 1 across original + replay", len(m.calls))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
