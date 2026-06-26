package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/localrouter"
)

func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return rc, mr
}

// ─── Publisher ───────────────────────────────────────────────────────────────

func TestPublisher_RoundTrip(t *testing.T) {
	rc, _ := newTestRedis(t)
	pub := newPublisher(rc, 0)

	snap := &NodeSnapshot{
		GeneratedAt: time.Now().UTC(),
		CacheNodes: []CacheNodeEntry{
			{ID: "c1", WorkspaceID: "ws1", URL: "http://cache1:6379", MaxSizeGB: 10},
		},
		EmbeddingNodes: []EmbeddingNodeEntry{
			{ID: "e1", WorkspaceID: "ws1", URL: "http://embed1:9092",
				Model: "text-embedding-3-small", Dimensions: 1536},
		},
	}

	if err := pub.Publish(context.Background(), snap); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got, err := pub.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil snapshot after publish")
	}
	if len(got.CacheNodes) != 1 || got.CacheNodes[0].ID != "c1" {
		t.Fatalf("unexpected cache nodes: %+v", got.CacheNodes)
	}
	if len(got.EmbeddingNodes) != 1 || got.EmbeddingNodes[0].Model != "text-embedding-3-small" {
		t.Fatalf("unexpected embedding nodes: %+v", got.EmbeddingNodes)
	}
}

func TestPublisher_Latest_NoSnapshot_ReturnsNil(t *testing.T) {
	rc, _ := newTestRedis(t)
	pub := newPublisher(rc, 0)
	snap, err := pub.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest with no snapshot should not error, got: %v", err)
	}
	if snap != nil {
		t.Fatalf("expected nil when no snapshot published, got %+v", snap)
	}
}

func TestPublisher_NilRedis_NoOp(t *testing.T) {
	pub := NewPublisher(nil, 0)
	if err := pub.Publish(context.Background(), &NodeSnapshot{}); err != nil {
		t.Fatalf("Publish with nil redis should not error, got: %v", err)
	}
	snap, err := pub.Latest(context.Background())
	if err != nil || snap != nil {
		t.Fatalf("Latest with nil redis: expected (nil, nil), got (%v, %v)", snap, err)
	}
}

func TestPublisher_TTLIsSet(t *testing.T) {
	rc, mr := newTestRedis(t)
	pub := newPublisher(rc, 0)
	if err := pub.Publish(context.Background(), &NodeSnapshot{GeneratedAt: time.Now()}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if ttl := mr.TTL(snapshotKey); ttl <= 0 {
		t.Fatalf("expected TTL > 0 on snapshot key, got %v", ttl)
	}
}

func TestPublisher_SecondPublishOverwrites(t *testing.T) {
	rc, _ := newTestRedis(t)
	pub := newPublisher(rc, 0)

	first := &NodeSnapshot{CacheNodes: []CacheNodeEntry{{ID: "c-first"}}}
	second := &NodeSnapshot{CacheNodes: []CacheNodeEntry{{ID: "c-second"}}}

	if err := pub.Publish(context.Background(), first); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if err := pub.Publish(context.Background(), second); err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	got, err := pub.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got.CacheNodes[0].ID != "c-second" {
		t.Fatalf("expected second snapshot to overwrite, got %+v", got.CacheNodes)
	}
}

// ─── NodeSyncer ──────────────────────────────────────────────────────────────

func TestNodeSyncer_RegistersInferenceNodes(t *testing.T) {
	rc, _ := newTestRedis(t)
	pub := newPublisher(rc, 0)

	snap := &NodeSnapshot{
		GeneratedAt: time.Now(),
		InferenceNodes: []InferenceNodeEntry{
			{ID: "n1", WorkspaceID: "ws1", URL: "http://node1:9090",
				Provider: "vllm", Models: []string{"llama3"}, MaxConcurrent: 4},
		},
	}
	if err := pub.Publish(context.Background(), snap); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	router := newFakeRegistry()
	syncer := &NodeSyncer{pub: pub, router: router}
	syncer.Sync(context.Background())

	if len(router.entries) != 1 || router.entries["n1"].URL != "http://node1:9090" {
		t.Fatalf("expected node n1 registered, got %+v", router.entries)
	}
}

func TestNodeSyncer_RemovesStaleNodes(t *testing.T) {
	rc, _ := newTestRedis(t)
	pub := newPublisher(rc, 0)

	router := newFakeRegistry()
	// Pre-register a stale mining endpoint and a static (WorkspaceID="") endpoint.
	router.entries["stale-1"] = fakeEntry{ID: "stale-1", WorkspaceID: "ws1"}
	router.entries["static-1"] = fakeEntry{ID: "static-1", WorkspaceID: ""}

	// Publish snapshot with no inference nodes → stale mining node should be removed.
	if err := pub.Publish(context.Background(), &NodeSnapshot{GeneratedAt: time.Now()}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	syncer := &NodeSyncer{pub: pub, router: router}
	syncer.Sync(context.Background())

	if _, ok := router.entries["stale-1"]; ok {
		t.Fatal("stale mining node should have been removed")
	}
	if _, ok := router.entries["static-1"]; !ok {
		t.Fatal("static endpoint should NOT have been removed")
	}
}

func TestNodeSyncer_NoSnapshot_NoChange(t *testing.T) {
	rc, _ := newTestRedis(t)
	pub := newPublisher(rc, 0)

	router := newFakeRegistry()
	router.entries["existing"] = fakeEntry{ID: "existing", WorkspaceID: "ws1"}

	// Nothing published — syncer should be a no-op.
	syncer := &NodeSyncer{pub: pub, router: router}
	syncer.Sync(context.Background())

	if len(router.entries) != 1 {
		t.Fatalf("expected router unchanged when no snapshot, got %d entries", len(router.entries))
	}
}

// ─── fakeRegistry ────────────────────────────────────────────────────────────

// fakeRegistry implements endpointRegistry for syncer tests without pulling in
// localrouter or starting health-check goroutines.

type fakeEntry struct {
	ID          string
	WorkspaceID string
	URL         string
}

type fakeRegistry struct {
	entries map[string]fakeEntry
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{entries: make(map[string]fakeEntry)}
}

// The fake uses the endpointRegistry interface — but NodeSyncer's concrete
// field is *localrouter.Router.  To keep tests self-contained we define the
// syncer via a helper that accepts the interface directly.

func (f *fakeRegistry) Register(e *localrouter.LocalEndpoint) {
	f.entries[e.ID] = fakeEntry{ID: e.ID, WorkspaceID: e.WorkspaceID, URL: e.URL}
}

func (f *fakeRegistry) Remove(id string) bool {
	_, ok := f.entries[id]
	delete(f.entries, id)
	return ok
}

func (f *fakeRegistry) List() []*localrouter.LocalEndpoint {
	out := make([]*localrouter.LocalEndpoint, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, &localrouter.LocalEndpoint{
			ID:          e.ID,
			WorkspaceID: e.WorkspaceID,
			URL:         e.URL,
		})
	}
	return out
}

func (f *fakeRegistry) CheckHealthByID(_ context.Context, _ string) (*localrouter.LocalEndpoint, error) {
	return nil, nil
}
