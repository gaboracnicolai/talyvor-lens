package cache

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/doc2query"
)

// AN EMPTY EXTRACTION MUST BE A REFUSAL, NOT A MATCH — over the real serve path.
//
// ⚠ WHY A SECOND FILE RATHER THAN A UNIT TEST. The production comparison is NOT
// discriminator.Match: it is `AND discriminators = $6` inside semanticSelectPooledSQL, with $6
// bound to Canon(prompt) in Go. Fixing Match alone would leave the SQL serving empty-to-empty
// exactly as before, and every unit test would be green. This is the seam that decides money.
//
// ⚠ THE PAIR IS REAL AND MEASURED, not invented for the test: notice-direction scores 0.9770 with
// text-embedding-3-small — above BOTH the old 0.92 and, at the time it was measured, the only
// thing standing between a landlord and a tenant getting each other's answer. Both sides
// canonicalise to "", so before this change the gate passed them having verified nothing.
//
// ⚠ THE EMBEDDER IS CONSTANT ON PURPOSE (see discriminator_pooling_realpg_test.go): similarity is
// pinned at 1.0, so the threshold cannot be the reason for a refusal. If this test passes, the
// gate is the only thing that can have refused.

const (
	landlordNotice = "How much notice does a landlord have to give before ending a tenancy?"
	tenantNotice   = "How much notice does a tenant have to give before ending a tenancy?"
)

func TestGetPooled_RefusesWhenNeitherPromptNamesAnEntity(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	vec, _ := constEmbedder{}.Embed(ctx, landlordNotice)
	if err := c.SetPooled(ctx, "anthropic", "claude-sonnet-5", landlordNotice, "ws-contributor",
		[]byte("the landlord must give two months"), vec); err != nil {
		t.Fatalf("SetPooled: %v", err)
	}

	resp, contributor, _, sim, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", tenantNotice)
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if resp != nil {
		t.Errorf("the pool served a LANDLORD answer to a TENANT question "+
			"(similarity %.4f, contributor %q, body %q).\n"+
			"Neither prompt names an extractable entity, so both canonicalise to \"\" and the SQL "+
			"gate `discriminators = $6` compared '' to '' and called it a match. The gate verified "+
			"nothing and reported a positive. Measured: this is how 97%% of consumer danger pairs "+
			"got through it.", sim, contributor, string(resp))
	}
}

// ⚠ THE ROWS THAT ARE ALREADY IN THE POOL — and the only test that can tell the read-side fix
// from the write-side one.
//
// Found by positive control C2: reverting GetPooled's refusal left every other test GREEN, because
// the write-side NULLIF meant the row under test was NULL and therefore unreachable regardless.
// The two seams mask each other for rows this merge writes. They do NOT mask each other for rows
// written BEFORE it, which is every poolable row in production today: those carry
// `discriminators = ”`, a real value, and `” = ”` is TRUE.
//
// This merge deliberately ships no data migration — with the read-side refusal in place an
// entity-less query never reaches the SQL, and an entity-bearing query cannot equal ” — so those
// legacy rows are unreachable rather than rewritten. That argument is only true while the refusal
// exists, which is precisely why it needs a test that fails without it.
//
// The row is inserted directly, bypassing SetPooled, because SetPooled can no longer produce this
// state. A fixture that cannot express the defect cannot guard against it.
func TestGetPooled_RefusesLegacyRowStoredWithEmptyStringDiscriminators(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	vec, _ := constEmbedder{}.Embed(ctx, landlordNotice)
	if _, err := pool.Exec(ctx, `INSERT INTO prompt_embeddings
	  (provider, model, prompt_hash, embedding, response, contributor_workspace_id,
	   is_poolable, embedding_model, discriminators)
	VALUES ($1, $2, $3, $4, $5, $6, true, $7, '')`,
		"anthropic", "claude-sonnet-5", "legacy-empty-discriminators-row",
		vectorLiteral(vec), "the landlord must give two months", "ws-contributor",
		constEmbedder{}.Model()); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	resp, _, _, sim, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", tenantNotice)
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if resp != nil {
		t.Errorf("a PRE-EXISTING pooled row with discriminators = '' served a landlord answer to a "+
			"tenant question (similarity %.4f, body %q).\n"+
			"Every poolable row written before this merge is in exactly this state, so the write-side "+
			"NULLIF does not protect them — only the read-side refusal does.", sim, string(resp))
	}
}

// ⚠ THE WRITE SEAM, which is the half a read-side fix leaves behind. A row whose prompt named no
// entity must not sit in the pool as a servable target: `discriminators` must be NULL, the value
// semanticSelectPooledSQL already documents as failing closed ("NULL = $6 is NULL, never TRUE").
// Storing the empty string makes it a real value that compares equal to another empty string.
func TestSetPooled_EmptyExtractionStoresNULLNotEmptyString(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	vec, _ := constEmbedder{}.Embed(ctx, landlordNotice)
	if err := c.SetPooled(ctx, "anthropic", "claude-sonnet-5", landlordNotice, "ws-contributor",
		[]byte("the landlord must give two months"), vec); err != nil {
		t.Fatalf("SetPooled: %v", err)
	}

	var isNull bool
	if err := pool.QueryRow(ctx,
		`SELECT discriminators IS NULL FROM prompt_embeddings WHERE is_poolable = true LIMIT 1`).
		Scan(&isNull); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !isNull {
		var got string
		_ = pool.QueryRow(ctx,
			`SELECT COALESCE(discriminators, '<null>') FROM prompt_embeddings WHERE is_poolable = true LIMIT 1`).
			Scan(&got)
		t.Errorf("a pooled row whose prompt names no entity stored discriminators = %q, want NULL. "+
			"An empty string is a VALUE and compares equal to another empty string, so this row is "+
			"a live match target for every entity-less question in the pool.", got)
	}
}

// ⚠ THE THIRD SEAM. There are THREE places an empty extraction reaches the discriminators column:
// the pooled upsert, the returning form built from it, and the doc2query VARIANT upsert — which is
// a separate INSERT with its own parameter list. A fix applied to the first two leaves variant rows
// sitting in the pool as empty-string match targets, and every test above stays green.
//
// Variants inherit the ORIGINAL's discriminators by design (see SetPooledWithVariants). When the
// original names no entity, that inheritance is of nothing, and the row must be NULL for the same
// reason its parent must be.
func TestSetPooledWithVariants_EmptyExtractionStoresNULLOnEveryVariantRow(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	vec, _ := constEmbedder{}.Embed(ctx, landlordNotice)
	variants := []doc2query.Variant{
		{Question: "What notice period applies at the end of a tenancy?", Embedding: vec},
		{Question: "How long before a tenancy ends must notice be given?", Embedding: vec},
	}
	if err := c.SetPooledWithVariants(ctx, "anthropic", "claude-sonnet-5", landlordNotice,
		"ws-contributor", []byte("two months"), vec, variants); err != nil {
		t.Fatalf("SetPooledWithVariants: %v", err)
	}

	var total, nonNull int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(discriminators) FROM prompt_embeddings WHERE variant_of IS NOT NULL`).
		Scan(&total, &nonNull); err != nil {
		t.Fatalf("query: %v", err)
	}
	if total != len(variants) {
		t.Fatalf("wrote %d variant rows, want %d — the fixture is not exercising the seam", total, len(variants))
	}
	if nonNull != 0 {
		t.Errorf("%d of %d variant rows stored a non-NULL discriminators for a prompt that names "+
			"no entity; each is a live match target for every entity-less question in the pool",
			nonNull, total)
	}
}

// ⚠ THE FLOOR, and it is not optional. Everything above is satisfied by a gate that refuses
// EVERYTHING, which would silently delete the product this repo exists to sell. An entity-bearing
// pair must still serve over the same real path.
//
// (TestGetPooled_StillServesWhenEntitiesMatch in discriminator_pooling_realpg_test.go is the
// sibling floor; this one is stated here too so a reader of THIS file cannot mistake the change
// for "pooling turned off".)
func TestGetPooled_EntityBearingPairStillServesAfterTheFix(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	stored := "How do I write a validator in Pydantic v2?"
	asked := "In Pydantic v2, how do I write a validator?"

	vec, _ := constEmbedder{}.Embed(ctx, stored)
	if err := c.SetPooled(ctx, "anthropic", "claude-sonnet-5", stored, "ws-contributor",
		[]byte("use @field_validator"), vec); err != nil {
		t.Fatalf("SetPooled: %v", err)
	}

	resp, _, _, _, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", asked)
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if resp == nil {
		t.Fatal("the pool refused a genuine rephrasing naming the SAME entities — fail-closed has " +
			"become fail-always, and cross-tenant pooling now serves nothing at all")
	}
}
