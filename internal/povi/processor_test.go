package povi

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
)

func staticLookup(pub ed25519.PublicKey) PubKeyLookup {
	return func(_ context.Context, _ string) (ed25519.PublicKey, error) { return pub, nil }
}

// A valid receipt with minting OFF (default): verified + recorded for audit,
// but NO mint.
func TestProcess_ValidReceipt_MintingOff_RecordsNoMint(t *testing.T) {
	pub, priv, _ := GenerateNodeKey()
	m := &fakeMinter{}
	p := NewProcessor(NewStore(nil), m, staticLookup(pub), false)

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
	p := NewProcessor(NewStore(nil), m, staticLookup(pub), true) // even with minting ON

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
	p := NewProcessor(NewStore(nil), m, lookup, true)

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
	p := NewProcessor(NewStore(nil), m, staticLookup(pub), true)

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
