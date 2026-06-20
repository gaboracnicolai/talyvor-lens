package main

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/povi"
)

func testSigner(t *testing.T) (*receiptSigner, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return &receiptSigner{
		priv:        priv,
		nodeID:      "node-1",
		workspaceID: "ws-1",
		traces:      povi.NewTraceCache(povi.DefaultTraceTTL),
		now:         func() time.Time { return time.Unix(1_700_000_000, 0) },
	}, pub
}

// TestSign_RunePath_TaggedRune — the LIVE production path must tag every receipt
// 'rune' from day one (don't ship an untagged production path), with one leaf per
// rune, and the receipt must still verify.
func TestSign_RunePath_TaggedRune(t *testing.T) {
	rs, pub := testSigner(t)
	rec := rs.sign("req-1", "llama", 3, 5, "héllo") // 5 runes (é is one rune)

	if rec.LeafKind != povi.LeafKindRune {
		t.Fatalf("production rune path LeafKind = %q, want %q (must tag from day one)", rec.LeafKind, povi.LeafKindRune)
	}
	if rec.LeafCount != 5 {
		t.Errorf("LeafCount = %d, want 5 (one per rune)", rec.LeafCount)
	}
	if err := povi.VerifyReceipt(rec, pub); err != nil {
		t.Errorf("VerifyReceipt (rune): %v", err)
	}
}

// TestSignTokens_TokenPath_TaggedToken — the per-token path yields one leaf per
// token (not per rune), tagged 'token', and verifies.
func TestSignTokens_TokenPath_TaggedToken(t *testing.T) {
	rs, pub := testSigner(t)
	tokens := []string{"hé", "llo", " wörld"} // 3 tokens, 11 runes
	rec := rs.signTokens("req-2", "llama", 3, 3, tokens)

	if rec.LeafKind != povi.LeafKindToken {
		t.Fatalf("token path LeafKind = %q, want %q", rec.LeafKind, povi.LeafKindToken)
	}
	if rec.LeafCount != len(tokens) {
		t.Errorf("LeafCount = %d, want %d tokens (NOT the 11 runes)", rec.LeafCount, len(tokens))
	}
	if err := povi.VerifyReceipt(rec, pub); err != nil {
		t.Errorf("VerifyReceipt (token): %v", err)
	}
}
