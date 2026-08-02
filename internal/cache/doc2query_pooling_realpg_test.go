package cache

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/doc2query"
)

// DOC2QUERY VARIANTS AS MATCH TARGETS — and the two properties that make them safe.
//
// A variant is an extra vector pointing at ONE stored answer. It widens recall: the person who
// phrased the question differently finds the answer through a target derived from the answer
// itself, rather than through the original asker's wording.
//
// ⚠ THE PROPERTY THE WHOLE DESIGN RESTS ON: a variant carries the ORIGINAL's discriminators, never
// its own. A model deriving questions from a Pydantic v2 answer will happily produce "how do I
// validate a field?" with no version in it. If that variant carried its own entities it would be a
// match target with NO version constraint pointing at version-specific content — doc2query would
// have opened the exact hole #392 closed, and opened it in the safest-looking way.
//
// These use the constant-vector embedder from discriminator_pooling_realpg_test.go: similarity is
// pinned at 1.0, so nothing here can pass or fail because of the threshold.

// ⚠ RED: a variant whose OWN text omits the version must not become a version-less match target.
func TestSetPooledWithVariants_VariantInheritsOriginalDiscriminators(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	original := "How do I write a validator in Pydantic v2?"
	answer := []byte("Use @field_validator, which replaced @validator in v2.")
	vec, _ := constEmbedder{}.Embed(ctx, original)

	// The middle variant deliberately drops the version — exactly what a deriving model produces.
	variants := []doc2query.Variant{
		{Question: "How do I validate a field in Pydantic v2?", Embedding: vec},
		{Question: "How do I validate a field?", Embedding: vec},
		{Question: "What replaced the validator decorator in Pydantic v2?", Embedding: vec},
	}
	if err := c.SetPooledWithVariants(ctx, "anthropic", "claude-sonnet-5", original, "ws-contributor", answer, vec, variants); err != nil {
		t.Fatalf("SetPooledWithVariants: %v", err)
	}

	// Someone asks the version-LESS question. The variant's own text would match it, but the
	// variant inherits "v2", so the entity gate must still refuse.
	resp, _, _, _, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", "How do I validate a field?")
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if resp != nil {
		t.Errorf("a version-less question was served version-specific content (%q) through a variant.\n"+
			"The variant's own text omits the version, so if it carried its OWN discriminators it "+
			"became an unconstrained match target for v2 prose. Variants must inherit the "+
			"original's entities — this is the hole doc2query would otherwise open.", string(resp))
	}

	// Control: the SAME question WITH the version must still be served, or the assertion above
	// passes for a system that simply stored nothing.
	resp2, _, _, _, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", "How do I validate a field in Pydantic v2?")
	if err != nil {
		t.Fatalf("GetPooled(control): %v", err)
	}
	if resp2 == nil {
		t.Fatal("the version-matching question was NOT served — variants were not written, or were " +
			"written unreachable, so the inheritance assertion above proves nothing")
	}
}

// ⚠ A VARIANT HIT SERVES THE CONTRIBUTOR'S EXACT BYTES. Variants are match targets; the answer is
// never modified, paraphrased or truncated. Serving generated text would ship prose no model wrote
// for that question, credited to someone who did not write it.
func TestSetPooledWithVariants_ServesTheOriginalAnswerUnmodified(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	answer := []byte("Use @field_validator, which replaced @validator in v2.")
	vec, _ := constEmbedder{}.Embed(ctx, "x")
	original := "How do I write a validator in Pydantic v2?"

	if err := c.SetPooledWithVariants(ctx, "anthropic", "claude-sonnet-5", original, "ws-contributor", answer, vec,
		[]doc2query.Variant{{Question: "In Pydantic v2 how is a field validated?", Embedding: vec}}); err != nil {
		t.Fatalf("SetPooledWithVariants: %v", err)
	}

	resp, contributor, entryID, _, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", "In Pydantic v2 how is a field validated?")
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if resp == nil {
		t.Fatal("variant did not match its own text at similarity 1.0")
	}
	if string(resp) != string(answer) {
		t.Errorf("served %q, want the contributor's exact bytes %q — a variant is a match target, "+
			"never a substitute for the answer", string(resp), string(answer))
	}
	// ⚠ ROYALTY ATTRIBUTION: the contributor is the workspace that wrote the ANSWER.
	if contributor != "ws-contributor" {
		t.Errorf("contributor = %q, want ws-contributor", contributor)
	}
	// ⚠ AND THE ENTRY ID MUST RESOLVE TO THE ORIGINAL ROW, not the variant's own. entry_id is what
	// poolroyalty/detector.go groups by (GROUP BY entry_id) for ring detection and rate limiting.
	// If each variant reported its own id, one answer would present as N independent entries and an
	// attacker could split hits across them to stay under every per-entry threshold.
	var originalID string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM prompt_embeddings WHERE is_poolable = true AND variant_of IS NULL`).Scan(&originalID); err != nil {
		t.Fatalf("look up original row: %v", err)
	}
	if entryID != originalID {
		t.Errorf("variant hit reported entry_id %q, want the ORIGINAL %q. Anti-gaming aggregation "+
			"groups by entry_id; N variants reporting N ids divides every count by N.", entryID, originalID)
	}
}

// The variant rows must actually carry the inherited value in the column, not merely behave as if
// they do for the one query shape above.
func TestSetPooledWithVariants_VariantRowsStoreTheInheritedDiscriminators(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	original := "How do I configure Tailwind CSS v4?"
	vec, _ := constEmbedder{}.Embed(ctx, original)
	want := string(discriminator.Canon(original))

	if err := c.SetPooledWithVariants(ctx, "anthropic", "claude-sonnet-5", original, "ws-c", []byte("edit the CSS file"), vec,
		[]doc2query.Variant{
			{Question: "How do I set up Tailwind?", Embedding: vec},
			{Question: "Where does Tailwind config live now?", Embedding: vec},
		}); err != nil {
		t.Fatalf("SetPooledWithVariants: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT discriminators FROM prompt_embeddings WHERE variant_of IS NOT NULL`)
	if err != nil {
		t.Fatalf("query variants: %v", err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var got string
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if got != want {
			t.Errorf("variant row stored discriminators %q, want the original's %q", got, want)
		}
	}
	if n != 2 {
		t.Errorf("found %d variant rows, want 2", n)
	}
}
