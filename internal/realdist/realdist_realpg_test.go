package realdist

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// ⚠ THIS HARNESS WILL BE RUN BY SOMEONE ELSE, AGAINST A DATABASE I CANNOT SEE, AND ITS OUTPUT WILL
// DECIDE WHETHER CROSS-TENANT POOLING IS A PRODUCT. An instrument handed over unverified is worse
// than no instrument: it returns a number that looks authoritative and cannot be checked.
//
// So it is positive-controlled against vectors whose similarities are known by construction. If
// the seeded 0.90 pair does not come back as 0.90, nothing the harness reports about real traffic
// means anything.

func realdistPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG realdist test")
	}
	ctx := context.Background()
	name := fmt.Sprintf("lens_realdist_%d", time.Now().UnixNano())

	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = ac.Close(ctx)
		t.Fatalf("create: %v", err)
	}
	_ = ac.Close(ctx)

	u, _ := url.Parse(admin)
	u.Path = "/" + name
	dsn := u.String()

	mc, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := dbmigrate.Run(ctx, mc, migrations.FS); err != nil {
		_ = mc.Close(ctx)
		t.Fatalf("migrate: %v", err)
	}
	_ = mc.Close(ctx)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(context.Background())
	})
	return pool
}

// unitAtAngle builds a 1536-d unit vector whose cosine with unitAtAngle(0) is exactly cos(theta).
func unitAtAngle(theta float64) []float32 {
	v := make([]float32, 1536)
	v[0] = float32(math.Cos(theta))
	v[1] = float32(math.Sin(theta))
	return v
}

func lit(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

type pgxQuerier struct{ p *pgxpool.Pool }

func (q pgxQuerier) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	r, err := q.p.Query(ctx, sql, args...)
	return r, err
}

func seed(t *testing.T, pool *pgxpool.Pool, hash string, v []float32, poolable bool, discrim string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO prompt_embeddings (provider, model, prompt_hash, embedding, response, is_poolable, embedding_model, discriminators, contributor_workspace_id)
		 VALUES ('anthropic','claude-sonnet-5',$1,$2,'r',$3,'text-embedding-3-small',$4,'ws-c')`,
		hash, lit(v), poolable, discrim)
	if err != nil {
		t.Fatalf("seed %s: %v", hash, err)
	}
}

// ⚠ THE CONTROL: vectors placed at known angles must come back at those similarities.
func TestMeasure_ReportsKnownSimilaritiesExactly(t *testing.T) {
	pool := realdistPool(t)
	// Three pooled prompts: two at cos=0.90 to each other, one far away at cos≈0.
	seed(t, pool, "a", unitAtAngle(0), true, "d1")
	seed(t, pool, "b", unitAtAngle(math.Acos(0.90)), true, "d1")
	seed(t, pool, "c", unitAtAngle(math.Pi/2), true, "d1")

	rows, err := Measure(context.Background(), pgxQuerier{pool})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	var best float64
	for _, r := range rows {
		if r.NearestSim > best {
			best = r.NearestSim
		}
	}
	if math.Abs(best-0.90) > 0.001 {
		t.Errorf("nearest-neighbour similarity = %.4f, want 0.9000. The harness cannot be trusted "+
			"to describe real traffic if it cannot reproduce a similarity built by construction.", best)
	}
}

// ⚠ AND IT MUST DISTINGUISH — a harness that reports everything as similar would "confirm" any
// hypothesis. The far vector's nearest neighbour is a genuine 0.90 pair, so check the isolated
// case directly: with only ONE pooled row, there is no neighbour and the answer must be 0.
func TestMeasure_SingleRowHasNoNeighbour(t *testing.T) {
	pool := realdistPool(t)
	seed(t, pool, "solo", unitAtAngle(0), true, "d1")
	rows, err := Measure(context.Background(), pgxQuerier{pool})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(rows) != 1 || rows[0].NearestSim != 0 {
		t.Errorf("solo row reported nearest=%v; a prompt with nothing to match must report 0, or "+
			"an empty pool would read as a similar one", rows)
	}
}

// ⚠ THE GATE COLUMN MUST ACTUALLY CONSTRAIN. Two vectors at 0.99 with DIFFERENT discriminators are
// exactly the Pydantic-v1/v2 shape: the ungated figure must see them, the gated figure must not.
func TestMeasure_GatedNeighbourRespectsDiscriminators(t *testing.T) {
	pool := realdistPool(t)
	seed(t, pool, "v1", unitAtAngle(0), true, "num:1|tech:pydantic")
	seed(t, pool, "v2", unitAtAngle(math.Acos(0.99)), true, "num:2|tech:pydantic")

	rows, err := Measure(context.Background(), pgxQuerier{pool})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	for _, r := range rows {
		if math.Abs(r.NearestSim-0.99) > 0.001 {
			t.Errorf("ungated nearest = %.4f, want 0.99 — the raw distribution must still see the pair", r.NearestSim)
		}
		if r.NearestGatedSim != 0 {
			t.Errorf("gated nearest = %.4f, want 0 — different discriminators must not count as a "+
				"reachable neighbour, or the gated column overstates what the pool can serve", r.NearestGatedSim)
		}
	}
}

// ⚠ PRIVATE AND POOLED MUST NOT BE MIXED. Private prompts are workspace-ID-prefixed, so their
// similarities are inflated; folding them in would report a pool that serves better than it does.
func TestMeasure_SeparatesPrivateFromPooled(t *testing.T) {
	pool := realdistPool(t)
	seed(t, pool, "p1", unitAtAngle(0), false, "d1")
	seed(t, pool, "p2", unitAtAngle(math.Acos(0.99)), false, "d1")
	seed(t, pool, "q1", unitAtAngle(0), true, "d1")
	seed(t, pool, "q2", unitAtAngle(math.Acos(0.70)), true, "d1")

	rows, err := Measure(context.Background(), pgxQuerier{pool})
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	for _, r := range rows {
		want := 0.99
		if r.Poolable {
			want = 0.70
		}
		if math.Abs(r.NearestSim-want) > 0.001 {
			t.Errorf("poolable=%v nearest=%.4f want %.4f — a private neighbour must never be "+
				"counted for a pooled row", r.Poolable, r.NearestSim, want)
		}
	}
}

// ⚠ THE PRIVACY BOUNDARY, ASSERTED RATHER THAN REVIEWED. The policy page says prompts are not
// retained by default; this harness must not be the thing that makes that false. It reads
// embedding-derived numbers only.
func TestSQL_SelectsNoContentColumns(t *testing.T) {
	sql := strings.ToLower(SQL())
	for _, banned := range []string{"response", "prompt_text", "prompt_hash"} {
		if strings.Contains(sql, banned) {
			t.Errorf("the query references %q. This harness exists precisely because prompt content "+
				"is not readable and must stay that way — reading content here would change the "+
				"privacy posture to obtain a measurement that does not need it.", banned)
		}
	}
}

// An empty database must be reported as unanswerable, never as a low-similarity finding.
func TestReport_EmptyIsNotAFinding(t *testing.T) {
	out := Report(nil, []float64{0.5, 0.8}, "synthetic")
	if !strings.Contains(out, "NO ROWS") {
		t.Errorf("an empty result rendered without a warning:\n%s", out)
	}
}
