package povi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// End-to-end transport: the Lens client SIGNS a challenge, the node VERIFIES it
// against the Lens pubkey and answers from its retained trace, and the client
// decodes proofs that verify against the committed root.
func TestChallengeClient_SignsAndFetches(t *testing.T) {
	lensPub, lensPriv, _ := GenerateNodeKey()

	st := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")}
	root := MerkleRoot(st)
	tc := NewTraceCache(time.Hour)
	tc.Put("req-1", st)

	// Stub node /challenge handler: verify Lens's signature, then answer.
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChallengeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := VerifyChallenge(req, lensPub); err != nil {
			http.Error(w, "bad challenge sig", http.StatusUnauthorized)
			return
		}
		lps, err := tc.SampledLeafProofs(req.RequestID, req.Positions)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(lps)
	}))
	defer node.Close()

	client := NewChallengeClient(lensPriv, 5*time.Second)
	answers, err := client.FetchPaths(context.Background(), "node-1", node.URL, "req-1", []int{0, 2, 4})
	if err != nil {
		t.Fatalf("FetchPaths: %v", err)
	}
	if len(answers) != 3 {
		t.Fatalf("got %d answers, want 3", len(answers))
	}
	for _, a := range answers {
		if !VerifyPath(root, a.Leaf, a.Proof) {
			t.Errorf("position %d did not verify against root", a.Position)
		}
	}
}

// A node that rejects an unsigned/forged challenge → client gets an error (which
// the Challenger treats as a failed challenge).
func TestChallengeClient_RejectedByNode(t *testing.T) {
	_, lensPriv, _ := GenerateNodeKey()
	otherPub, _, _ := GenerateNodeKey() // node pins a DIFFERENT key

	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChallengeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := VerifyChallenge(req, otherPub); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = ed25519.PublicKey(nil)
	}))
	defer node.Close()

	client := NewChallengeClient(lensPriv, 5*time.Second)
	if _, err := client.FetchPaths(context.Background(), "n", node.URL, "req-1", []int{0}); err == nil {
		t.Error("expected an error when the node rejects the challenge")
	}
}
