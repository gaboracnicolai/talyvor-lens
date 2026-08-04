package cache

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// A POOLED HIT MUST NOT CROSS AN ENTITY BOUNDARY.
//
// ⚠ THE MEASURED DEFECT. On engineering traffic with text-embedding-3-small, "How do I write a
// validator in Pydantic v1?" and "…v2?" score 0.9579 — ABOVE the production threshold of 0.92.
// Three more pairs do the same (Vue 2/3 at 0.9450, Tailwind v3/v4 at 0.9265, React Router v5/v6 at
// 0.9207). Meanwhile the best GENUINE rephrasing measured 0.8488. The populations are inverted by
// 0.109, so no threshold separates them: every value that serves one rephrasing admits a wrong
// answer first.
//
// The two questions are genuinely about the same topic — the embedding is not wrong to score them
// alike. What differs is one digit, against which the two correct answers are incompatible. So the
// fix is a SEPARATE exact test on entities, not a tuning of the similarity one.
//
// ⚠ THE EMBEDDER HERE RETURNS THE SAME VECTOR FOR EVERY PROMPT, DELIBERATELY. That pins similarity
// at 1.0, which removes the threshold as a possible explanation for a refusal. If these tests pass
// with a constant embedder, the ONLY thing that can have refused the match is the discriminator
// gate. A test using the real embedder would pass at 0.92 for the uninteresting reason that
// nothing clears the threshold at all.

// constEmbedder returns one fixed vector regardless of input: cosine similarity is exactly 1.0
// between any two prompts, so the threshold can never be the reason for a miss.
type constEmbedder struct{ model string }

func (c constEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	v := make([]float32, 1536)
	v[0] = 1
	return v, nil
}
func (c constEmbedder) Model() string {
	if c.model == "" {
		return "text-embedding-3-small"
	}
	return c.model
}

func discrimPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG discriminator test")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_discrim_%d", time.Now().UnixNano())

	adminConn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := adminConn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = adminConn.Close(ctx)
		t.Fatalf("create %s: %v", name, err)
	}
	_ = adminConn.Close(ctx)

	dsn := discrimSwapDB(admin, name)
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
		ac, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = ac.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = ac.Close(context.Background())
	})
	return pool
}

func discrimSwapDB(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// ⚠ THE CASE THAT STARTED THIS, over the real serve path against real Postgres: tenant A
// contributes a Pydantic v1 answer, tenant B asks the v2 question. Same provider, same model, same
// embedding model, similarity pinned at 1.0. The pool must refuse.
func TestGetPooled_RefusesAcrossVersionBoundary(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	stored := "How do I write a validator in Pydantic v1?"
	asked := "How do I write a validator in Pydantic v2?"

	vec, _ := constEmbedder{}.Embed(ctx, stored)
	if err := c.SetPooled(ctx, "anthropic", "claude-sonnet-5", stored, "ws-contributor", []byte("use @validator"), vec); err != nil {
		t.Fatalf("SetPooled: %v", err)
	}

	resp, contributor, _, sim, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", asked)
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if resp != nil {
		t.Errorf("pool served a v1 answer to a v2 question (similarity %.4f, contributor %q, body %q).\n"+
			"These score 0.9579 with the production embedder — ABOVE the 0.92 threshold — so nothing "+
			"in the similarity path can refuse it. The answer is confidently wrong, served "+
			"cross-tenant on a paid path, and credited as a royalty to someone who answered a "+
			"different question.", sim, contributor, string(resp))
	}
}

// ⚠ AND THE POSITIVE CONTROL, or the test above passes for a system that pools NOTHING. Same
// entities on both sides must still hit — otherwise the gate has simply disabled the product.
func TestGetPooled_StillServesWhenEntitiesMatch(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	stored := "How do I write a validator in Pydantic v2?"
	asked := "In Pydantic v2, what is the way to write a validator?"

	vec, _ := constEmbedder{}.Embed(ctx, stored)
	if err := c.SetPooled(ctx, "anthropic", "claude-sonnet-5", stored, "ws-contributor", []byte("use @field_validator"), vec); err != nil {
		t.Fatalf("SetPooled: %v", err)
	}

	resp, contributor, _, _, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", asked)
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if resp == nil {
		t.Fatal("pool refused two prompts naming the SAME entities (Pydantic, v2) at similarity 1.0. " +
			"The gate is supposed to separate entity mismatches from rephrasings; refusing this " +
			"means it has disabled pooling rather than made it safe.")
	}
	if contributor != "ws-contributor" {
		t.Errorf("contributor = %q, want ws-contributor — a variant match must still credit the "+
			"workspace that actually wrote the answer", contributor)
	}
}

// ⚠ LEGACY ROWS FAIL CLOSED. Rows written before this change carry NULL discriminators, and
// prompt_embeddings stores only prompt_hash — the prompt TEXT is gone, so they can never be
// re-derived. An unverifiable row must not be served rather than be given the benefit of the doubt.
func TestGetPooled_LegacyRowWithoutDiscriminatorsIsNotServed(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	// Write a poolable row the way the pre-gate code did: no discriminators column value.
	_, err := pool.Exec(ctx,
		`INSERT INTO prompt_embeddings
		   (provider, model, prompt_hash, embedding, response, contributor_workspace_id, is_poolable, embedding_model)
		 VALUES ($1,$2,$3,$4,$5,$6,true,$7)`,
		"anthropic", "claude-sonnet-5", "legacy-hash-1",
		vectorLiteral(func() []float32 { v, _ := constEmbedder{}.Embed(ctx, ""); return v }()),
		"legacy answer", "ws-old", "text-embedding-3-small")
	if err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	resp, _, _, _, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", "anything at all")
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if resp != nil {
		t.Errorf("a legacy row with NULL discriminators was served (%q). Its prompt text no longer "+
			"exists, so nothing can establish which entities it was about — the only safe reading "+
			"of an unverifiable row is that it does not match.", string(resp))
	}
}

// THE GLUED-UNIT BLIND SPOT — measured on the real serve path, not on Match().
//
// reNum required a word boundary AFTER the digits (`\b[vV]?(\d+(?:\.\d+)*)\b`), so a unit glued to
// the number swallowed it whole: "512mb" extracted NO number at all, while "512 mb" extracted
// num:512. The gate built to catch numeric mismatches was blind to the most common way engineers
// write them, and these pairs sit at the same similarity as pydantic v1/v2 — above the threshold,
// so nothing in the similarity path can refuse them either.
//
// ⚠ IT IS A FALSE-ALLOW **AND** A FALSE-REFUSE. The same question written "512mb" and "512 mb"
// extracted DIFFERENT discriminators, so a genuine rephrasing between the two forms was refused for
// a spurious reason. The fix normalises both forms to the same token, which is why the second test
// below is not optional.
func TestGetPooled_RefusesGluedUnitMismatch(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	for _, tc := range []struct{ name, stored, asked string }{
		{"jvm-heap", "How do I set the JVM heap to 512mb?", "How do I set the JVM heap to 2048mb?"},
		{"timeout", "Why does my request time out after 30s?", "Why does my request time out after 60s?"},
		{"dose", "Can I take 400mg of ibuprofen?", "Can I take 800mg of ibuprofen?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vec, _ := constEmbedder{}.Embed(ctx, tc.stored)
			if err := c.SetPooled(ctx, "anthropic", "claude-sonnet-5", tc.stored, "ws-contributor",
				[]byte("stored answer"), vec); err != nil {
				t.Fatalf("SetPooled: %v", err)
			}
			resp, contributor, _, sim, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", tc.asked)
			if err != nil {
				t.Fatalf("GetPooled: %v", err)
			}
			if resp != nil {
				t.Errorf("pool served %q in answer to %q (similarity %.4f, contributor %q, body %q).\n"+
					"The numbers differ and the unit is glued to them, so reNum's trailing \\b "+
					"discarded the number entirely and the discriminators compared equal. Served "+
					"cross-tenant on a paid path, and credited as a royalty to someone who answered "+
					"a different question.", tc.stored, tc.asked, sim, contributor, string(resp))
			}
		})
	}
}

// ⚠ THE SPACING POSITIVE CONTROL — the false-REFUSE half.
//
// "512mb" and "512 mb" are the same question. Before the fix they extracted different
// discriminators and the pool refused a genuine rephrasing; after it they must normalise to the
// same token and still pool. Without this test the fix could "pass" by making every glued number
// its own opaque token, which trades a false-allow for a false-refuse.
func TestGetPooled_GluedAndSpacedUnitsAreTheSameQuestion(t *testing.T) {
	pool := discrimPool(t)
	ctx := context.Background()
	c := NewSemanticCache(pool, constEmbedder{}, 0.92, 24*time.Hour)

	stored := "How do I set the JVM heap to 512mb?"
	asked := "How do I set the JVM heap to 512 mb?"

	vec, _ := constEmbedder{}.Embed(ctx, stored)
	if err := c.SetPooled(ctx, "anthropic", "claude-sonnet-5", stored, "ws-contributor",
		[]byte("-Xmx512m"), vec); err != nil {
		t.Fatalf("SetPooled: %v", err)
	}
	resp, _, _, sim, err := c.GetPooled(ctx, "anthropic", "claude-sonnet-5", asked)
	if err != nil {
		t.Fatalf("GetPooled: %v", err)
	}
	if resp == nil {
		t.Errorf("the pool REFUSED a genuine rephrasing (similarity %.4f): %q vs %q differ only in "+
			"whether the unit is glued to the number, and must normalise to the same discriminator",
			sim, stored, asked)
	}
}
