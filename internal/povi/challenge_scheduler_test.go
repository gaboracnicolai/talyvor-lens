package povi

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeReceiptLister struct {
	recs []StoredReceipt
}

func (f *fakeReceiptLister) ListVerifiedReceipts(_ context.Context, _ int) ([]StoredReceipt, error) {
	return f.recs, nil
}

func recsFixture(n int) []StoredReceipt {
	out := make([]StoredReceipt, n)
	for i := 0; i < n; i++ {
		st := steps(10)
		root := MerkleRoot(st)
		id := "req-" + string(rune('a'+i))
		out[i] = StoredReceipt{RequestID: id, NodeID: "node-1", WorkspaceID: "ws", MerkleRootHex: hexRoot(root), Verified: true, LeafCount: len(st)}
	}
	return out
}

func hexRoot(r [32]byte) string {
	const hexd = "0123456789abcdef"
	b := make([]byte, 64)
	for i, c := range r {
		b[i*2] = hexd[c>>4]
		b[i*2+1] = hexd[c&0xf]
	}
	return string(b)
}

// Rate 1.0 → every verified receipt is challenged this round.
func TestScheduler_RateOneChallengesAll(t *testing.T) {
	lister := &fakeReceiptLister{recs: recsFixture(5)}
	// honest provider over all fixtures so challenges PASS (no slashing here).
	prov := &multiProvider{traces: map[string][][]byte{}}
	for _, r := range lister.recs {
		prov.traces[r.RequestID] = steps(10)
	}
	c, _, _ := newTestChallenger(t, prov)
	sched := NewChallengeScheduler(c, lister, 1.0)

	issued := sched.RunOnce(context.Background())
	if issued != 5 {
		t.Errorf("rate 1.0 should challenge all 5, got %d", issued)
	}
}

// Rate 0 → nothing challenged.
func TestScheduler_RateZeroChallengesNone(t *testing.T) {
	lister := &fakeReceiptLister{recs: recsFixture(5)}
	c, _, _ := newTestChallenger(t, &multiProvider{traces: map[string][][]byte{}})
	sched := NewChallengeScheduler(c, lister, 0.0)
	if issued := sched.RunOnce(context.Background()); issued != 0 {
		t.Errorf("rate 0 should challenge none, got %d", issued)
	}
}

// The scheduler goroutine exits promptly on context cancel.
func TestScheduler_StopsOnCancel(t *testing.T) {
	lister := &fakeReceiptLister{}
	c, _, _ := newTestChallenger(t, &multiProvider{traces: map[string][][]byte{}})
	sched := NewChallengeScheduler(c, lister, 0.5)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sched.StartScheduler(ctx, 10*time.Millisecond); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop on cancel")
	}
}

// multiProvider answers honestly for many requests.
type multiProvider struct {
	mu     sync.Mutex
	traces map[string][][]byte
}

func (p *multiProvider) FetchPaths(_ context.Context, _, _, requestID string, positions []int) ([]LeafProof, error) {
	p.mu.Lock()
	st := p.traces[requestID]
	p.mu.Unlock()
	tc := NewTraceCache(time.Hour)
	tc.Put(requestID, st)
	return tc.SampledLeafProofs(requestID, positions)
}
