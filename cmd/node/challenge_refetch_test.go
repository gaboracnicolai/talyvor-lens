package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/povi"
)

// lensPubkeyServer simulates Lens's GET /v1/povi/pubkey with a controllable
// current key + an outage switch, so one server can drive pin/rotate/fail.
type lensPubkeyServer struct {
	mu   sync.Mutex
	key  ed25519.PublicKey
	fail bool
}

func (l *lensPubkeyServer) set(k ed25519.PublicKey) {
	l.mu.Lock()
	l.key, l.fail = k, false
	l.mu.Unlock()
}
func (l *lensPubkeyServer) down() { l.mu.Lock(); l.fail = true; l.mu.Unlock() }
func (l *lensPubkeyServer) handler(w http.ResponseWriter, _ *http.Request) {
	l.mu.Lock()
	k, f := l.key, l.fail
	l.mu.Unlock()
	if f {
		http.Error(w, "lens unavailable", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"ed25519_pubkey": povi.EncodePublicKey(k)})
}

func newRefetchFixture(t *testing.T) (*lensPubkeyServer, *LensClient, *InferenceServer, func()) {
	t.Helper()
	lens := &lensPubkeyServer{}
	ts := httptest.NewServer(http.HandlerFunc(lens.handler))
	client := NewLensClient(ts.URL, "k")
	srv := NewInferenceServer(fakeProvider{resp: InferResponse{Text: "x"}}, "", NodeConfig{})
	return lens, client, srv, ts.Close
}

// Pin → same-key-no-change → rotate → failed-fetch-keeps-last-known-good.
func TestRefreshChallengeKey_PinRotateAndLastKnownGood(t *testing.T) {
	pubA, _, _ := povi.GenerateNodeKey()
	pubB, _, _ := povi.GenerateNodeKey()
	lens, client, srv, done := newRefetchFixture(t)
	defer done()

	// 1. first fetch pins A
	lens.set(pubA)
	refreshChallengeKey(context.Background(), client, srv)
	if !bytes.Equal(srv.challengeKey(), pubA) {
		t.Fatalf("should pin key A")
	}
	// 2. same key again → still A (no spurious change)
	refreshChallengeKey(context.Background(), client, srv)
	if !bytes.Equal(srv.challengeKey(), pubA) {
		t.Fatalf("same key should leave A pinned")
	}
	// 3. Lens rotates to B → node re-pins B
	lens.set(pubB)
	refreshChallengeKey(context.Background(), client, srv)
	if !bytes.Equal(srv.challengeKey(), pubB) {
		t.Fatalf("should rotate to key B")
	}
	// 4. Lens goes down → node KEEPS last-known-good (B), does not drop it
	lens.down()
	refreshChallengeKey(context.Background(), client, srv)
	if !bytes.Equal(srv.challengeKey(), pubB) {
		t.Fatalf("a failed re-fetch must keep the last-known-good key (B), got %x", srv.challengeKey())
	}
}

// Fail-closed preserved: a node that has NEVER successfully fetched stays
// unpinned (so handleChallenge refuses).
func TestRefreshChallengeKey_FailClosedWhenNeverFetched(t *testing.T) {
	lens, client, srv, done := newRefetchFixture(t)
	defer done()
	lens.down() // unavailable from the start

	refreshChallengeKey(context.Background(), client, srv)
	if len(srv.challengeKey()) != 0 {
		t.Errorf("node should remain unpinned (fail-closed) when it never fetched a key")
	}
}
