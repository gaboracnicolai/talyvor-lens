package povi

import (
	"errors"
	"sync"
	"time"
)

// TraceBuilder accumulates per-step leaves during generation. In this part the
// node calls AddStep once per OUTPUT RUNE (the documented stand-in for a true
// per-token step — see the merkle.go note); Root folds the Merkle root that
// goes into the receipt. Building the root is O(n) over the trace and runs once
// at completion, off the per-token hot loop.
type TraceBuilder struct {
	steps [][]byte
}

func NewTraceBuilder() *TraceBuilder { return &TraceBuilder{} }

// AddStep appends one trace step, copying the input so a caller reusing a
// scratch buffer can't corrupt the retained trace.
func (b *TraceBuilder) AddStep(step []byte) {
	cp := make([]byte, len(step))
	copy(cp, step)
	b.steps = append(b.steps, cp)
}

// Len is the number of steps accumulated.
func (b *TraceBuilder) Len() int { return len(b.steps) }

// Root folds the Merkle root over the accumulated trace.
func (b *TraceBuilder) Root() [32]byte { return MerkleRoot(b.steps) }

// Steps returns the accumulated trace (for retention in a TraceCache).
func (b *TraceBuilder) Steps() [][]byte { return b.steps }

// DefaultTraceTTL bounds how long a node retains a trace so it can answer a
// later challenge with sampled paths (Part 3). Past this, the trace is dropped.
const DefaultTraceTTL = 30 * time.Minute

// maxTraceCacheEntries bounds memory: the cache never holds more than this many
// traces, evicting expired ones first and then the next-to-expire.
const maxTraceCacheEntries = 10000

type traceEntry struct {
	steps     [][]byte
	expiresAt time.Time
}

// TraceCache retains generation traces keyed by RequestID for a bounded TTL so
// the challenge layer (Part 3) can produce sampled authentication paths. It is
// concurrency-safe. The node owns one; Lens does not (Lens only holds receipts,
// not full traces).
type TraceCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]traceEntry
	now     func() time.Time // injectable for tests
}

// NewTraceCache builds a cache with the given retention TTL (<=0 uses the
// default).
func NewTraceCache(ttl time.Duration) *TraceCache {
	if ttl <= 0 {
		ttl = DefaultTraceTTL
	}
	return &TraceCache{
		ttl:     ttl,
		entries: make(map[string]traceEntry),
		now:     time.Now,
	}
}

// Put retains a trace for requestID until now+ttl. It sweeps expired entries
// and enforces the size bound.
func (c *TraceCache) Put(requestID string, steps [][]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	if len(c.entries) >= maxTraceCacheEntries {
		c.evictOneLocked()
	}
	c.entries[requestID] = traceEntry{steps: steps, expiresAt: c.now().Add(c.ttl)}
}

// SampledPaths returns authentication paths for the requested leaf indices of a
// retained trace — exactly what the Part 3 challenge will sample. Errors if the
// trace is unknown/expired or an index is out of range.
func (c *TraceCache) SampledPaths(requestID string, indices []int) ([]Proof, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[requestID]
	if !ok || !c.now().Before(e.expiresAt) {
		if ok {
			delete(c.entries, requestID) // drop expired
		}
		return nil, errors.New("povi: trace not retained for request (unknown or expired)")
	}
	proofs := make([]Proof, 0, len(indices))
	for _, idx := range indices {
		p, err := BuildProof(e.steps, idx)
		if err != nil {
			return nil, err
		}
		proofs = append(proofs, p)
	}
	return proofs, nil
}

// SampledLeafProofs returns the leaf value + authentication path for each
// requested position — the node's answer to a Part-3 challenge. Errors if the
// trace is unknown/expired or an index is out of range.
func (c *TraceCache) SampledLeafProofs(requestID string, indices []int) ([]LeafProof, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[requestID]
	if !ok || !c.now().Before(e.expiresAt) {
		if ok {
			delete(c.entries, requestID)
		}
		return nil, errors.New("povi: trace not retained for request (unknown or expired)")
	}
	out := make([]LeafProof, 0, len(indices))
	for _, idx := range indices {
		p, err := BuildProof(e.steps, idx)
		if err != nil {
			return nil, err
		}
		leaf := make([]byte, len(e.steps[idx]))
		copy(leaf, e.steps[idx])
		out = append(out, LeafProof{Position: idx, Leaf: leaf, Proof: p})
	}
	return out, nil
}

// Len is the current number of retained traces (after no sweep).
func (c *TraceCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *TraceCache) sweepLocked() {
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}

// evictOneLocked drops the soonest-to-expire entry to keep memory bounded when
// the cache is full of still-live traces.
func (c *TraceCache) evictOneLocked() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, e := range c.entries {
		if first || e.expiresAt.Before(oldest) {
			oldestKey, oldest, first = k, e.expiresAt, false
		}
	}
	if !first {
		delete(c.entries, oldestKey)
	}
}
