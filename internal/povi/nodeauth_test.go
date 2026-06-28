package povi

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func bodyHashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func mustSignTok(t *testing.T, priv ed25519.PrivateKey, node, req, bh string, exp int64) string {
	t.Helper()
	tok, err := SignNodeAuthToken(priv, node, req, bh, exp)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A correctly-signed, correctly-bound, unexpired token verifies.
func TestNodeAuthToken_RoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bh := bodyHashOf(`{"model":"trial-mock","messages":[{"role":"user","content":"hi"}]}`)
	tok := mustSignTok(t, priv, "node-1", "req-1", bh, time.Now().Add(30*time.Second).Unix())
	if err := VerifyNodeAuthToken(tok, pub, "node-1", bh, time.Now()); err != nil {
		t.Fatalf("valid token must verify: %v", err)
	}
}

// Every binding is load-bearing: wrong key, wrong body, wrong node, or expiry → reject.
func TestNodeAuthToken_Rejections(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	bh := bodyHashOf(`{"model":"trial-mock","messages":[{"role":"user","content":"hi"}]}`)
	exp := time.Now().Add(30 * time.Second).Unix()

	// (a) signed by a key that is NOT the pinned Lens key.
	if VerifyNodeAuthToken(mustSignTok(t, wrongPriv, "node-1", "req-1", bh, exp), pub, "node-1", bh, time.Now()) == nil {
		t.Error("wrong-key token must be rejected")
	}
	// (b) token bound to a DIFFERENT body — the anti-arbitrary-inference binding.
	otherBH := bodyHashOf(`{"model":"trial-mock","messages":[{"role":"user","content":"DIFFERENT"}]}`)
	if VerifyNodeAuthToken(mustSignTok(t, priv, "node-1", "req-1", otherBH, exp), pub, "node-1", bh, time.Now()) == nil {
		t.Error("body-hash mismatch must be rejected")
	}
	// (c) token minted for a DIFFERENT node — no cross-node reuse.
	if VerifyNodeAuthToken(mustSignTok(t, priv, "node-2", "req-1", bh, exp), pub, "node-1", bh, time.Now()) == nil {
		t.Error("node_id mismatch must be rejected")
	}
	// (d) expired beyond the skew grace.
	if VerifyNodeAuthToken(mustSignTok(t, priv, "node-1", "req-1", bh, time.Now().Add(-time.Minute).Unix()), pub, "node-1", bh, time.Now()) == nil {
		t.Error("expired token must be rejected")
	}
	// (e) garbage / not a token.
	if VerifyNodeAuthToken("not-base64!!", pub, "node-1", bh, time.Now()) == nil {
		t.Error("garbage token must be rejected")
	}
}

// A token a few seconds past exp still verifies (NTP-close skew grace), but minutes past does not.
func TestNodeAuthToken_SkewGrace(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bh := bodyHashOf("body")
	// 3s past exp, within the 5s grace → valid.
	if err := VerifyNodeAuthToken(mustSignTok(t, priv, "n", "r", bh, time.Now().Add(-3*time.Second).Unix()), pub, "n", bh, time.Now()); err != nil {
		t.Errorf("token within skew grace should verify: %v", err)
	}
	// 30s past exp, beyond grace → expired.
	if VerifyNodeAuthToken(mustSignTok(t, priv, "n", "r", bh, time.Now().Add(-30*time.Second).Unix()), pub, "n", bh, time.Now()) == nil {
		t.Error("token well past exp must be rejected")
	}
}
