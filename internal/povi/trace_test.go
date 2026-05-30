package povi

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestTraceBuilder_RootMatchesMerkleRoot(t *testing.T) {
	b := NewTraceBuilder()
	steps := [][]byte{[]byte("x"), []byte("y"), []byte("z")}
	for _, s := range steps {
		b.AddStep(s)
	}
	if b.Len() != 3 {
		t.Fatalf("Len = %d, want 3", b.Len())
	}
	if b.Root() != MerkleRoot(steps) {
		t.Error("TraceBuilder.Root must equal MerkleRoot over the same steps")
	}
}

// AddStep must copy its input — a caller reusing a scratch buffer must not
// corrupt retained steps.
func TestTraceBuilder_CopiesInput(t *testing.T) {
	b := NewTraceBuilder()
	scratch := []byte("a")
	b.AddStep(scratch)
	scratch[0] = 'b'
	root := b.Root()
	if root != MerkleRoot([][]byte{[]byte("a")}) {
		t.Error("AddStep did not copy its input — retained step was mutated")
	}
}

// The cache retains a trace so a later challenge (Part 3) can produce sampled
// authentication paths that verify against the receipt's root.
func TestTraceCache_RetainAndSampledPaths(t *testing.T) {
	c := NewTraceCache(time.Minute)
	steps := [][]byte{[]byte("t0"), []byte("t1"), []byte("t2"), []byte("t3")}
	root := MerkleRoot(steps)
	c.Put("req-1", steps)

	proofs, err := c.SampledPaths("req-1", []int{0, 3})
	if err != nil {
		t.Fatalf("SampledPaths: %v", err)
	}
	if len(proofs) != 2 {
		t.Fatalf("got %d proofs, want 2", len(proofs))
	}
	if !VerifyPath(root, steps[0], proofs[0]) || !VerifyPath(root, steps[3], proofs[1]) {
		t.Error("sampled paths did not verify against the retained trace's root")
	}
}

func TestTraceCache_MissingAndOutOfRange(t *testing.T) {
	c := NewTraceCache(time.Minute)
	if _, err := c.SampledPaths("nope", []int{0}); err == nil {
		t.Error("missing request should error")
	}
	c.Put("req", [][]byte{[]byte("a")})
	if _, err := c.SampledPaths("req", []int{5}); err == nil {
		t.Error("out-of-range index should error")
	}
}

// A retained trace expires after its TTL (bounded retention).
func TestTraceCache_Expiry(t *testing.T) {
	c := NewTraceCache(time.Minute)
	base := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return base }
	c.Put("req", [][]byte{[]byte("a"), []byte("b")})

	// Within TTL → present.
	if _, err := c.SampledPaths("req", []int{0}); err != nil {
		t.Fatalf("within TTL should be present: %v", err)
	}
	// Past TTL → evicted.
	c.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, err := c.SampledPaths("req", []int{0}); err == nil {
		t.Error("expired trace should be gone")
	}
}

// Concurrent Put/SampledPaths must be race-free (run under -race).
func TestTraceCache_Concurrent(t *testing.T) {
	c := NewTraceCache(time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("req-%d", i)
			c.Put(id, [][]byte{[]byte("a"), []byte("b"), []byte("c")})
			_, _ = c.SampledPaths(id, []int{0, 2})
			_ = c.Len()
		}()
	}
	wg.Wait()
}
