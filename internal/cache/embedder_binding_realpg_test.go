package cache_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// SILENT CACHE CORRUPTION — and this is NOT a privacy finding.
//
// prompt_embeddings recorded no provenance for its vectors: the `model` column is the
// COMPLETION model (claude-sonnet, gpt-4o), never the EMBEDDER. So changing
// LENS_EMBEDDING_MODEL left vectors from the old embedder and the new one sitting in one
// table, being compared to each other by the very same similarity query.
//
// Embeddings from different models occupy DIFFERENT VECTOR SPACES. A cosine between them
// is not "shifted" — it is meaningless, and it can land arbitrarily high by coincidence.
// When it clears the threshold, the cache serves the OLD entry's response to a completely
// unrelated new query. Wrong answer, returned as a hit, with no error anywhere.
//
// ⚠ THIS BITES A SINGLE-TENANT SELF-HOSTER WITH POOLING OFF. It needs no second workspace
// and no cross-tenant sharing — only an operator changing the embedding model for cost or
// latency, which is an ordinary thing to do and which nothing warned against.
//
// The test asserts the SERVED BYTES, not a return code: the failure is that the wrong
// answer comes back, so that is what is checked.

// realPGPool provisions its OWN database rather than sharing lens_test.
//
// The shared database is destructively reset by other packages mid-run (observed: schema
// dropped to a single migration row while this package was executing), so a test that
// migrates it and then reads from it is racing something it does not control. An isolated
// database removes the coupling entirely and costs one CREATE/DROP.
func realPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	// Derive a unique name; the admin connection cannot create a database from inside a
	// transaction, so this runs on its own connection.
	name := fmt.Sprintf("lens_cachebind_%d", time.Now().UnixNano())
	adminConn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := adminConn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = adminConn.Close(ctx)
		t.Fatalf("create %s: %v", name, err)
	}
	_ = adminConn.Close(ctx)

	dsn := swapDBName(admin, name)
	migConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	if _, err := dbmigrate.Run(ctx, migConn, migrations.FS); err != nil {
		_ = migConn.Close(ctx)
		t.Fatalf("migrate %s: %v", name, err)
	}
	_ = migConn.Close(ctx)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		ac, cerr := pgx.Connect(context.Background(), admin)
		if cerr != nil {
			return
		}
		_, _ = ac.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = ac.Close(context.Background())
	})
	return pool
}

// swapDBName replaces the database path in a postgres DSN.
func swapDBName(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// fixedEmbedder returns one constant vector regardless of input, standing in for "whatever
// direction this model happens to map text into". Two DIFFERENT models are simulated by two
// instances whose vectors are close together — which is precisely the cross-space
// coincidence that makes this dangerous: nothing about the texts is similar, but the
// numbers are.
type fixedEmbedder struct {
	name string
	vec  []float32
}

func (f fixedEmbedder) Embed(context.Context, string) ([]float32, error) { return f.vec, nil }
func (f fixedEmbedder) Model() string                                    { return f.name }

func vec(dim int, lead float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = 0.01
	}
	v[0] = lead
	return v
}

// RED before the fix: embedder B's query matches embedder A's stored row and is served A's
// response. GREEN after: the row is not comparable, so it is not served.
func TestSemantic_VectorFromAnotherEmbedder_IsNotServed(t *testing.T) {
	pool := realPGPool(t)
	ctx := context.Background()

	const (
		dim       = 1536
		promptA   = "how do I reconcile a ledger entry in Go"
		promptB   = "what dose of contrast for a paediatric CT"
		responseA = "ANSWER-FROM-EMBEDDER-A"
	)

	// Model A writes an entry.
	embA := fixedEmbedder{name: "text-embedding-3-small", vec: vec(dim, 1.0)}
	cacheA := cache.NewSemanticCacheWithDB(pool, embA, 0.92, time.Hour)
	vA, _ := embA.Embed(ctx, promptA)
	if err := cacheA.Set(ctx, "anthropic", "claude-sonnet-4-6", promptA, []byte(responseA), vA, "ws-1"); err != nil {
		t.Fatalf("Set under embedder A: %v", err)
	}

	// The operator swaps LENS_EMBEDDING_MODEL. Model B maps a COMPLETELY UNRELATED prompt
	// to a nearby vector — a coincidence across two different spaces, not a similarity of
	// meaning.
	embB := fixedEmbedder{name: "text-embedding-ada-002", vec: vec(dim, 1.0001)}
	cacheB := cache.NewSemanticCacheWithDB(pool, embB, 0.92, time.Hour)

	got, err := cacheB.Get(ctx, "anthropic", "claude-sonnet-4-6", promptB, "ws-1")
	if err != nil {
		t.Fatalf("Get under embedder B: %v", err)
	}

	if string(got) == responseA {
		t.Fatalf("SILENT CACHE CORRUPTION: a query embedded by %q was served the response stored "+
			"under %q.\n  served: %q\n  the two prompts are unrelated (%q vs %q); only the VECTORS "+
			"coincided, because they come from different spaces and were compared anyway.",
			embB.Model(), embA.Model(), got, promptB, promptA)
	}
	if got != nil {
		t.Fatalf("expected a miss across embedders, got %q", got)
	}
}

// The same corruption on the cross-tenant path. Kept separate because a self-hoster with
// pooling off is exposed to the test above and NOT to this one — conflating them is how
// this reads as a privacy issue when it is a correctness issue.
func TestSemanticPooled_VectorFromAnotherEmbedder_IsNotServed(t *testing.T) {
	pool := realPGPool(t)
	ctx := context.Background()
	const dim = 1536

	embA := fixedEmbedder{name: "text-embedding-3-small", vec: vec(dim, 1.0)}
	cacheA := cache.NewSemanticCacheWithDB(pool, embA, 0.92, time.Hour)
	vA, _ := embA.Embed(ctx, "contributor prompt")
	if err := cacheA.SetPooled(ctx, "anthropic", "claude-sonnet-4-6",
		cache.PooledPromptKey("contributor prompt"), "ws-owner", []byte("POOLED-FROM-A"), vA); err != nil {
		t.Fatalf("SetPooled: %v", err)
	}

	embB := fixedEmbedder{name: "text-embedding-ada-002", vec: vec(dim, 1.0001)}
	cacheB := cache.NewSemanticCacheWithDB(pool, embB, 0.92, time.Hour)

	got, owner, _, _, err := cacheB.GetPooled(ctx, "anthropic", "claude-sonnet-4-6", "unrelated requester prompt")
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if string(got) == "POOLED-FROM-A" {
		t.Fatalf("SILENT CROSS-TENANT CORRUPTION: pooled row from embedder %q served to a query "+
			"embedded by %q (owner=%s, served=%q)", embA.Model(), embB.Model(), owner, got)
	}
	if got != nil {
		t.Fatalf("expected a pooled miss across embedders, got %q", got)
	}
}

// The SAME embedder must still hit — otherwise the fix is indistinguishable from breaking
// the cache, and "no corruption" would be satisfied by serving nothing ever.
func TestSemantic_SameEmbedder_StillServes(t *testing.T) {
	pool := realPGPool(t)
	ctx := context.Background()
	const dim = 1536

	emb := fixedEmbedder{name: "text-embedding-3-small", vec: vec(dim, 1.0)}
	c := cache.NewSemanticCacheWithDB(pool, emb, 0.92, time.Hour)
	v, _ := emb.Embed(ctx, "a prompt")
	if err := c.Set(ctx, "anthropic", "claude-sonnet-4-6", "a prompt", []byte("THE-ANSWER"), v, "ws-1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := c.Get(ctx, "anthropic", "claude-sonnet-4-6", "a prompt", "ws-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "THE-ANSWER" {
		t.Fatalf("same-embedder hit lost: got %q, want THE-ANSWER — the binding must not break caching", got)
	}
}

// Pre-existing rows carry NULL provenance: written before the column existed, by an
// embedder nobody recorded. They must be treated as not-comparable, not assumed current.
func TestSemantic_LegacyNullProvenanceRow_IsNotServed(t *testing.T) {
	pool := realPGPool(t)
	ctx := context.Background()
	const dim = 1536

	emb := fixedEmbedder{name: "text-embedding-3-small", vec: vec(dim, 1.0)}
	c := cache.NewSemanticCacheWithDB(pool, emb, 0.92, time.Hour)
	v, _ := emb.Embed(ctx, "legacy prompt")

	if err := c.Set(ctx, "anthropic", "claude-sonnet-4-6", "legacy prompt", []byte("LEGACY-ANSWER"), v, "ws-1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Simulate a pre-0110 row: provenance unknown.
	if _, err := pool.Exec(ctx, `UPDATE prompt_embeddings SET embedding_model = NULL`); err != nil {
		t.Fatalf("null out provenance: %v", err)
	}

	got, err := c.Get(ctx, "anthropic", "claude-sonnet-4-6", "legacy prompt", "ws-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("a row with UNKNOWN embedder provenance was served (%q). Unknown must mean "+
			"not-comparable: assuming it came from the current model asserts a fact nobody recorded.", got)
	}
}
