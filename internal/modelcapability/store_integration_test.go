package modelcapability

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/worktier"
)

func mcTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG modelcapability test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS model_capability_observations`,
		`CREATE TABLE model_capability_observations (
			id BIGSERIAL PRIMARY KEY, model TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '',
			difficulty INTEGER NOT NULL, quality DOUBLE PRECISION NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

// tierForDifficulty returns Classify inputs (input tokens, complexity score) whose
// resulting WorkTier maps — via H1's DifficultyOfWorkTier — to exactly d in [0,6].
// This is how the seeder proves the H2→H1 binding: difficulty is never a bare int,
// it is derived from an H1 classification of representative traffic.
func tierForDifficulty(d int) (inputTokens, complexityScore int) {
	// (size rank, complexity rank) pairs summing to d, using distinct hardness shapes.
	shapes := map[int][2]int{
		0: {500, 0},    // small + trivial
		1: {500, 1},    // small + simple
		2: {2000, 1},   // medium + simple
		3: {2000, 3},   // medium + moderate
		4: {20000, 3},  // large + moderate
		5: {20000, 5},  // large + complex
		6: {120000, 5}, // xlarge + complex
	}
	s := shapes[d]
	return s[0], s[1]
}

// mintTripwireTables — the H2 path (Record + Fit) must never write to any canonical
// economy sink. A capability curve is analytics, never a mint.
var mintTripwireTables = []string{
	"lens_token_balances", "token_events", "traffic_mint_holds", "held_mint_adjudications", "lxc_ledger",
}

// setupMintTripwires creates each canonical mint sink FRESH and EMPTY, fully
// qualified into THIS test's private schema — so under -p 1 an unqualified name can
// never resolve to a shared public economy table another package created.
func setupMintTripwires(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	for _, tbl := range mintTripwireTables {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (id BIGSERIAL PRIMARY KEY)`, schema, tbl)); err != nil {
			t.Fatalf("create tripwire %s: %v", tbl, err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`TRUNCATE %s.%s`, schema, tbl)); err != nil {
			t.Fatalf("truncate tripwire %s: %v", tbl, err)
		}
	}
	return schema
}

func assertNoMint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) {
	t.Helper()
	for _, tbl := range mintTripwireTables {
		var n int
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.%s`, schema, tbl)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("mint/ledger table %s has %d rows — H2 must never mint", tbl, n)
		}
	}
}

// TestStore_Fit_ReflectsSeededQualityVsDifficulty_RealPG — the headline H2 proof.
// Seed two models' representative traffic on real Postgres — a DEGRADER whose
// quality falls 0.10 per difficulty step and a HOLDER whose quality is flat — with
// each observation's difficulty derived from an H1 WorkTier classification. Fit
// must produce ONE curve per model and the fitted slopes must reflect the seeding:
// the degrader's slope is clearly negative, the holder's ≈ 0, and degrader < holder.
func TestStore_Fit_ReflectsSeededQualityVsDifficulty_RealPG(t *testing.T) {
	pool := mcTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	// Three symmetric samples per difficulty ⇒ the per-difficulty mean is exactly
	// the trend value, while still being multi-sample "representative" traffic.
	seed := func(model string, qualityAt func(d int) float64) {
		for d := 0; d <= MaxDifficulty; d++ {
			in, score := tierForDifficulty(d)
			wt := worktier.Classify(in, 0, 0, score, false, false, "full")
			if got := DifficultyOfWorkTier(wt); got != d {
				t.Fatalf("seed shape for d=%d classified to difficulty %d", d, got)
			}
			mean := qualityAt(d)
			for _, q := range []float64{mean - 0.02, mean, mean + 0.02} {
				if err := store.RecordServed(ctx, model, "openai", wt, q); err != nil {
					t.Fatalf("RecordServed: %v", err)
				}
			}
		}
	}
	seed("degrader", func(d int) float64 { return 0.95 - 0.10*float64(d) })
	seed("holder", func(d int) float64 { return 0.80 })

	curves, err := store.Fit(ctx)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if len(curves) != 2 {
		t.Fatalf("got %d curves, want one per model (2)", len(curves))
	}
	byModel := map[string]Curve{}
	for _, c := range curves {
		byModel[c.Model] = c
	}
	deg, ok := byModel["degrader"]
	if !ok {
		t.Fatal("no curve for degrader")
	}
	hold, ok := byModel["holder"]
	if !ok {
		t.Fatal("no curve for holder")
	}

	// Each curve spans all 7 difficulties with the seeded per-difficulty means.
	if len(deg.Points) != MaxDifficulty+1 || len(hold.Points) != MaxDifficulty+1 {
		t.Fatalf("curves must span %d difficulties; got degrader=%d holder=%d", MaxDifficulty+1, len(deg.Points), len(hold.Points))
	}
	for _, p := range deg.Points {
		if want := 0.95 - 0.10*float64(p.Difficulty); math.Abs(p.AvgQuality-want) > 1e-6 {
			t.Errorf("degrader point d=%d avg=%v, want %v", p.Difficulty, p.AvgQuality, want)
		}
		if p.Samples != 3 {
			t.Errorf("degrader point d=%d samples=%d, want 3", p.Difficulty, p.Samples)
		}
	}
	if deg.N != 3*(MaxDifficulty+1) {
		t.Errorf("degrader N=%d, want %d", deg.N, 3*(MaxDifficulty+1))
	}

	// The fitted slopes reflect the seeded quality-vs-difficulty relationship.
	if math.Abs(deg.Slope-(-0.10)) > 0.01 {
		t.Errorf("degrader slope = %v, want ≈ -0.10", deg.Slope)
	}
	if math.Abs(hold.Slope) > 0.01 {
		t.Errorf("holder slope = %v, want ≈ 0 (quality holds)", hold.Slope)
	}
	if !(deg.Slope < hold.Slope-0.05) {
		t.Errorf("degrader slope %v must be clearly steeper (more negative) than holder %v", deg.Slope, hold.Slope)
	}
	// Intercept ≈ quality at difficulty 0.
	if math.Abs(deg.Intercept-0.95) > 0.02 {
		t.Errorf("degrader intercept = %v, want ≈ 0.95", deg.Intercept)
	}
}

// TestStore_Fit_NoMint_RealPG — the H2 path writes ONLY descriptive observations:
// Record persists to model_capability_observations and Fit is READ-ONLY (adds no
// rows). Every canonical mint/ledger sink stays at zero rows.
func TestStore_Fit_NoMint_RealPG(t *testing.T) {
	pool := mcTestPool(t)
	ctx := context.Background()
	schema := setupMintTripwires(t, ctx, pool)
	store := NewStore(pool)
	for d := 0; d <= MaxDifficulty; d++ {
		in, score := tierForDifficulty(d)
		wt := worktier.Classify(in, 0, 0, score, false, false, "full")
		if err := store.RecordServed(ctx, "m", "openai", wt, 0.5); err != nil {
			t.Fatal(err)
		}
	}
	countObs := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM model_capability_observations`).Scan(&n); err != nil {
			t.Fatalf("count observations: %v", err)
		}
		return n
	}
	before := countObs()
	if before != MaxDifficulty+1 {
		t.Fatalf("seeded rows = %d, want %d", before, MaxDifficulty+1)
	}
	if _, err := store.Fit(ctx); err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if after := countObs(); after != before {
		t.Errorf("Fit changed observation count %d → %d — Fit must be read-only", before, after)
	}
	assertNoMint(t, ctx, pool, schema)
}

// TestStore_NilPool_NoOp — a nil-pool store is inert.
func TestStore_NilPool_NoOp(t *testing.T) {
	s := NewStore(nil)
	if err := s.Record(context.Background(), "m", "openai", 3, 0.5); err != nil {
		t.Errorf("nil Record must no-op, got %v", err)
	}
	if err := s.RecordServed(context.Background(), "m", "openai", worktier.WorkTier{}, 0.5); err != nil {
		t.Errorf("nil RecordServed must no-op, got %v", err)
	}
	if got, err := s.Fit(context.Background()); err != nil || got != nil {
		t.Errorf("nil Fit must be empty no-op, got %v err=%v", got, err)
	}
}
