package povi

import (
	"crypto/ed25519"
	"testing"
)

// Lens signs a challenge; the node verifies it against Lens's pubkey — the
// symmetric mirror of Part 1 (node signs receipts, Lens verifies). This stops
// arbitrary callers from extracting a node's served-response content.
func TestSignVerifyChallenge_Roundtrip(t *testing.T) {
	lensPub, lensPriv, _ := GenerateNodeKey()
	ch := SignChallenge(lensPriv, "req-1", []int{3, 7, 11}, 1748600000)
	if len(ch.Signature) != ed25519.SignatureSize {
		t.Fatalf("sig size = %d", len(ch.Signature))
	}
	if err := VerifyChallenge(ch, lensPub); err != nil {
		t.Errorf("valid challenge failed to verify: %v", err)
	}
}

func TestVerifyChallenge_TamperedFails(t *testing.T) {
	lensPub, lensPriv, _ := GenerateNodeKey()
	ch := SignChallenge(lensPriv, "req-1", []int{3, 7, 11}, 1748600000)

	mut := map[string]func(*ChallengeRequest){
		"requestID": func(c *ChallengeRequest) { c.RequestID = "req-evil" },
		"positions": func(c *ChallengeRequest) { c.Positions = []int{3, 7, 12} },
		"nonce":     func(c *ChallengeRequest) { c.Nonce++ },
	}
	for name, m := range mut {
		bad := ch
		bad.Positions = append([]int{}, ch.Positions...)
		m(&bad)
		if err := VerifyChallenge(bad, lensPub); err == nil {
			t.Errorf("tampering %s did not break verification", name)
		}
	}
}

func TestVerifyChallenge_WrongKeyFails(t *testing.T) {
	_, lensPriv, _ := GenerateNodeKey()
	otherPub, _, _ := GenerateNodeKey()
	ch := SignChallenge(lensPriv, "req-1", []int{1, 2}, 100)
	if err := VerifyChallenge(ch, otherPub); err == nil {
		t.Error("challenge verified under the wrong key")
	}
}
