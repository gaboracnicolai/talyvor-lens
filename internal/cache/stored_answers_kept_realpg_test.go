package cache_test

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/quality"
)

// B21.2: a stored answer is kept until someone deletes it. At the default retention (0 — see
// config.TestLoad_SemanticCacheRetentionDefaultsToKeepForever) an answer last used 30 days ago is
// still served, private and pooled, and the sweeper deletes neither. Quality eviction still does.
func TestStoredAnswer_UnusedFor30Days_IsServedAndNotSwept(t *testing.T) {
	pool := realPGPool(t)
	ctx := context.Background()

	const (
		dim      = 1536
		provider = "anthropic"
		model    = "claude-sonnet-4-6"
		fp       = "test-fp"
		private  = "how do I enable replication in Postgres 16"
		shared   = "how do I rotate a Stripe webhook secret"
	)
	emb := fixedEmbedder{name: "text-embedding-3-small", vec: vec(dim, 1.0)}
	c := cache.NewSemanticCacheWithDB(pool, emb, 0.92, 0)
	v, _ := emb.Embed(ctx, private)

	if err := c.Set(ctx, provider, model, "ws-a:"+private, cache.SingleTurn(private), fp, []byte("PRIVATE-ANSWER"), v, "ws-a"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.SetPooled(ctx, provider, model, cache.PooledPromptKey(shared), cache.SingleTurn(shared), fp, "ws-a", []byte("SHARED-ANSWER"), v); err != nil {
		t.Fatalf("SetPooled: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE prompt_embeddings SET updated_at = NOW() - INTERVAL '30 days'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := c.DeleteStale(ctx)
	if err != nil {
		t.Fatalf("DeleteStale: %v", err)
	}
	if n != 0 {
		t.Fatalf("the sweeper deleted %d answers unused for 30 days; want 0", n)
	}

	got, err := c.Get(ctx, provider, model, cache.SingleTurn(private), fp, "ws-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "PRIVATE-ANSWER" {
		t.Fatalf("private answer unused for 30 days: served %q, want PRIVATE-ANSWER", got)
	}
	got, _, _, _, err = c.GetPooled(ctx, provider, model, cache.SingleTurn(shared), fp)
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if string(got) != "SHARED-ANSWER" {
		t.Fatalf("shared answer unused for 30 days: served %q, want SHARED-ANSWER", got)
	}

	// Quality eviction still deletes: the private row is gone and no longer served.
	var hash string
	if err := pool.QueryRow(ctx, `SELECT prompt_hash FROM prompt_embeddings WHERE workspace_id = 'ws-a'`).Scan(&hash); err != nil {
		t.Fatalf("read private row: %v", err)
	}
	if err := quality.New(pool).EvictLowQuality(ctx, hash); err != nil {
		t.Fatalf("EvictLowQuality: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM prompt_embeddings WHERE prompt_hash = $1`, hash).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Fatalf("evicted answer still stored (%d rows)", left)
	}
	if got, _ := c.Get(ctx, provider, model, cache.SingleTurn(private), fp, "ws-a"); got != nil {
		t.Fatalf("evicted answer still served: %q", got)
	}
}
