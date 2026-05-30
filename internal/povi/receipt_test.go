package povi

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"testing"
)

func sampleReceipt() Receipt {
	return Receipt{
		RequestID:    "req-1",
		NodeID:       "node-1",
		WorkspaceID:  "ws-1",
		Model:        "llama-3.1-8b",
		InputTokens:  42,
		OutputTokens: 128,
		MerkleRoot:   MerkleRoot([][]byte{[]byte("a"), []byte("b")}),
		Timestamp:    1748600000,
	}
}

// Sign then verify against the node's public key — the happy path.
func TestSignVerify_Roundtrip(t *testing.T) {
	pub, priv, err := GenerateNodeKey()
	if err != nil {
		t.Fatalf("GenerateNodeKey: %v", err)
	}
	r := SignReceipt(priv, sampleReceipt())
	if len(r.Signature) != ed25519.SignatureSize {
		t.Fatalf("signature size = %d", len(r.Signature))
	}
	if err := VerifyReceipt(r, pub); err != nil {
		t.Errorf("valid receipt failed to verify: %v", err)
	}
}

// Tampering ANY signed field after signing must break verification — this is
// the tamper-evidence the layer provides.
func TestVerify_TamperedFieldFails(t *testing.T) {
	pub, priv, _ := GenerateNodeKey()
	r := SignReceipt(priv, sampleReceipt())

	mutate := map[string]func(*Receipt){
		"OutputTokens": func(x *Receipt) { x.OutputTokens++ },
		"InputTokens":  func(x *Receipt) { x.InputTokens-- },
		"Model":        func(x *Receipt) { x.Model = "evil-model" },
		"WorkspaceID":  func(x *Receipt) { x.WorkspaceID = "ws-evil" },
		"RequestID":    func(x *Receipt) { x.RequestID = "req-evil" },
		"NodeID":       func(x *Receipt) { x.NodeID = "node-evil" },
		"Timestamp":    func(x *Receipt) { x.Timestamp += 1 },
		"MerkleRoot":   func(x *Receipt) { x.MerkleRoot[0] ^= 0xFF },
	}
	for field, m := range mutate {
		bad := r
		bad.MerkleRoot = r.MerkleRoot // value copy already; mutate below
		m(&bad)
		if err := VerifyReceipt(bad, pub); err == nil {
			t.Errorf("tampering %s did not break verification", field)
		}
	}
}

// A receipt signed by one node must not verify under a different node's key.
func TestVerify_WrongKeyFails(t *testing.T) {
	_, priv, _ := GenerateNodeKey()
	otherPub, _, _ := GenerateNodeKey()
	r := SignReceipt(priv, sampleReceipt())
	if err := VerifyReceipt(r, otherPub); err == nil {
		t.Error("receipt verified under the wrong node key")
	}
}

// Canonical serialization is deterministic: same receipt → same bytes (so the
// same signature is reproducible and verification is stable).
func TestCanonicalPayload_Deterministic(t *testing.T) {
	r := sampleReceipt()
	if !bytes.Equal(CanonicalPayload(r), CanonicalPayload(r)) {
		t.Error("canonical payload is not deterministic")
	}
	// The signature is NOT part of the signed payload.
	withSig := r
	withSig.Signature = []byte("anything")
	if !bytes.Equal(CanonicalPayload(r), CanonicalPayload(withSig)) {
		t.Error("signature must not affect the canonical payload")
	}
	// A different field → different payload (length-prefixing prevents
	// ambiguity between adjacent fields).
	other := r
	other.NodeID = r.NodeID + "x"
	if bytes.Equal(CanonicalPayload(r), CanonicalPayload(other)) {
		t.Error("distinct receipts produced identical payloads")
	}
}

// Field-boundary ambiguity guard: moving a byte across the RequestID|NodeID
// boundary must change the payload (length-prefixing, not naive concatenation).
func TestCanonicalPayload_NoBoundaryAmbiguity(t *testing.T) {
	a := sampleReceipt()
	a.RequestID, a.NodeID = "ab", "c"
	b := sampleReceipt()
	b.RequestID, b.NodeID = "a", "bc"
	if bytes.Equal(CanonicalPayload(a), CanonicalPayload(b)) {
		t.Error("naive concatenation ambiguity: ab|c collided with a|bc")
	}
}

func TestVerify_BadPubKeyLength(t *testing.T) {
	_, priv, _ := GenerateNodeKey()
	r := SignReceipt(priv, sampleReceipt())
	if err := VerifyReceipt(r, ed25519.PublicKey{1, 2, 3}); err == nil {
		t.Error("a malformed public key must produce an error, not a panic")
	}
}

// A signed receipt must still verify after a JSON round-trip — this is the
// node→Lens wire path (node encodes InferResponse/receipt, Lens decodes it).
func TestReceipt_JSONRoundTripVerifies(t *testing.T) {
	pub, priv, _ := GenerateNodeKey()
	signed := SignReceipt(priv, sampleReceipt())

	blob, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Receipt
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := VerifyReceipt(got, pub); err != nil {
		t.Errorf("receipt failed to verify after JSON round-trip: %v", err)
	}
}

func TestEncodeDecodePublicKey(t *testing.T) {
	pub, _, _ := GenerateNodeKey()
	enc := EncodePublicKey(pub)
	got, err := DecodePublicKey(enc)
	if err != nil {
		t.Fatalf("DecodePublicKey: %v", err)
	}
	if !bytes.Equal(got, pub) {
		t.Error("public key did not round-trip through encode/decode")
	}
	if _, err := DecodePublicKey("not-base64!!"); err == nil {
		t.Error("invalid encoding should error")
	}
}
