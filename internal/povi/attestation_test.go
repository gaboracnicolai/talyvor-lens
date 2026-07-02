package povi

import (
	"crypto/ed25519"
	"testing"
)

// (proof 3, unit) the node-identity ed25519 WRAP round-trips: a node signs (node_id|nonce|eat), the gateway
// verifies against the node's registered pubkey; any tamper (eat / nonce / node_id / wrong key) fails.
func TestAttestation_SignVerify_RoundTrip(t *testing.T) {
	pub, priv, err := GenerateNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	const eat = "mock.eat.jwt"
	resp := SignAttestation(priv, "node-1", 4242, eat)
	if resp.NodeID != "node-1" || resp.Nonce != 4242 || resp.EAT != eat {
		t.Fatalf("response fields not carried: %+v", resp)
	}
	if err := VerifyAttestation(resp, pub); err != nil {
		t.Fatalf("valid attestation must verify, got %v", err)
	}

	// tamper the EAT ⇒ the node signature no longer covers it ⇒ verify fails.
	bad := resp
	bad.EAT = "swapped.eat.jwt"
	if VerifyAttestation(bad, pub) == nil {
		t.Fatal("tampered EAT must fail node-signature verification")
	}
	// tamper the nonce ⇒ fails (replay/rebind guard).
	bad = resp
	bad.Nonce = 9999
	if VerifyAttestation(bad, pub) == nil {
		t.Fatal("tampered nonce must fail verification")
	}
	// a different node's key ⇒ fails.
	otherPub, _, _ := GenerateNodeKey()
	if VerifyAttestation(resp, otherPub) == nil {
		t.Fatal("verification under the wrong node key must fail")
	}
	// wrong-length key ⇒ error, not panic.
	if VerifyAttestation(resp, ed25519.PublicKey{1, 2, 3}) == nil {
		t.Fatal("short pubkey must error")
	}
}
