package worktier

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// representativeRequest is one point in the tier space, with the buckets the
// classifier must produce for it (proved by a real read-back) and the pre-serve
// signals the Advisor reasons from.
type representativeRequest struct {
	name                          string
	in, out                       int
	cost                          float64
	score                         int
	pii, guardrail                bool
	policy                        string
	model                         string
	wantSize                      SizeBucket
	wantCost                      CostBucket
	wantComplexity                Complexity
	wantSensitivity               Sensitivity
	wantDowngrade, wantSensOptOut bool
}

// representativeTraffic spans every axis edge — the "representative inputs" the
// classifier and the Advisor are proved on.
func representativeTraffic() []representativeRequest {
	return []representativeRequest{
		{"small/trivial/normal", 500, 100, 0.0005, 0, false, false, "full", "gpt-4o-mini",
			SizeSmall, CostTrivial, ComplexityTrivial, SensitivityNormal, true, false},
		{"small/simple/normal", 900, 99, 0.0009, 2, false, false, "full", "gpt-4o-mini",
			SizeSmall, CostTrivial, ComplexitySimple, SensitivityNormal, true, false},
		{"medium/low/normal", 2000, 500, 0.005, 1, false, false, "metadata", "gpt-4o",
			SizeMedium, CostLow, ComplexitySimple, SensitivityNormal, false, false},
		{"large/moderate/elevated-pii", 50000, 200, 0.05, 4, true, false, "metadata", "gpt-4o",
			SizeLarge, CostModerate, ComplexityModerate, SensitivityElevated, false, true},
		{"xlarge/high/normal", 120000, 5000, 0.5, 3, false, false, "full", "gpt-5.4",
			SizeXLarge, CostHigh, ComplexityModerate, SensitivityNormal, false, false},
		{"small/trivial/elevated-guardrail", 300, 50, 0.0001, 0, false, true, "full", "gpt-4o-mini",
			SizeSmall, CostTrivial, ComplexityTrivial, SensitivityElevated, true, true},
		{"medium/moderate/restricted", 3000, 800, 0.02, 3, false, false, "none", "gpt-4o",
			SizeMedium, CostModerate, ComplexityModerate, SensitivityRestricted, false, true},
		{"large/complex/restricted", 20000, 1000, 0.15, 5, false, false, "none", "gpt-5.4",
			SizeLarge, CostHigh, ComplexityComplex, SensitivityRestricted, false, true},
	}
}

// mintTripwireTables are the canonical economy sinks. The H1 path (classify +
// advise) must NEVER write a row to any of them — that is the "no mint call
// anywhere in the path" invariant, proved on real PG below.
var mintTripwireTables = []string{
	"lens_token_balances", "token_events", "traffic_mint_holds", "held_mint_adjudications", "lxc_ledger",
}

// setupMintTripwires creates each canonical mint sink FRESH and EMPTY, fully
// qualified into THIS test's private schema. Qualifying is the robustness point:
// under -p 1 the package binaries share one database, so an UNQUALIFIED name could
// resolve to a public economy table another package created (e.g. lxc_ledger with
// its append-only trigger). A schema-qualified name is created and read in our own
// schema only, so the tripwire can never touch — or be perturbed by — public state.
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

// assertNoMint fails if any qualified mint sink gained a row.
func assertNoMint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string) {
	t.Helper()
	for _, tbl := range mintTripwireTables {
		var n int
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.%s`, schema, tbl)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("mint/ledger table %s has %d rows — the path must never mint", tbl, n)
		}
	}
}

// countRows counts an unqualified table in the current (private) schema.
func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestAdvisor_ClassifierOnRepresentativeInputs_RealPG — proves the classifier on
// representative inputs: each request is Classify'd, persisted, and read BACK from
// real Postgres with its four derived buckets and raw signal columns asserted.
func TestAdvisor_ClassifierOnRepresentativeInputs_RealPG(t *testing.T) {
	pool := wtTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)
	for _, r := range representativeTraffic() {
		t.Run(r.name, func(t *testing.T) {
			wt := Classify(r.in, r.out, r.cost, r.score, r.pii, r.guardrail, r.policy)
			if wt.Size != r.wantSize || wt.Cost != r.wantCost || wt.Complexity != r.wantComplexity || wt.Sensitivity != r.wantSensitivity {
				t.Fatalf("Classify = %+v, want size=%s cost=%s cx=%s sens=%s", wt, r.wantSize, r.wantCost, r.wantComplexity, r.wantSensitivity)
			}
			if err := store.Record(ctx, "ws-rep", "feat", r.model, "openai", wt, r.in, r.out, r.cost, r.score, r.pii, r.guardrail); err != nil {
				t.Fatalf("Record: %v", err)
			}
			var sz, cost, cx, sens string
			var in, out, score int
			var usd float64
			if err := pool.QueryRow(ctx, `SELECT size_bucket, cost_bucket, complexity, sensitivity, input_tokens, output_tokens, cost_usd, complexity_score
				FROM work_tier_observations WHERE workspace_id='ws-rep' AND model=$1 AND input_tokens=$2 ORDER BY id DESC LIMIT 1`,
				r.model, r.in).Scan(&sz, &cost, &cx, &sens, &in, &out, &usd, &score); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if SizeBucket(sz) != r.wantSize || CostBucket(cost) != r.wantCost || Complexity(cx) != r.wantComplexity || Sensitivity(sens) != r.wantSensitivity {
				t.Errorf("persisted buckets %s/%s/%s/%s, want %s/%s/%s/%s", sz, cost, cx, sens, r.wantSize, r.wantCost, r.wantComplexity, r.wantSensitivity)
			}
			if in != r.in || out != r.out || score != r.score || usd != r.cost {
				t.Errorf("raw columns not persisted: in=%d out=%d score=%d usd=%v", in, out, score, usd)
			}
		})
	}
}

// TestAdvisor_AdvisoryOnly_NoMint_RealPG — the headline H1 proof. Across the
// representative traffic, on real Postgres: (1) the Advisor's advice is produced
// and matches the derived tier; (2) the DESCRIPTIVE classifier persists exactly
// one row per request; (3) EVERY canonical mint/ledger table stays at zero rows —
// the whole H1 path writes only descriptive metadata, never a mint.
func TestAdvisor_AdvisoryOnly_NoMint_RealPG(t *testing.T) {
	pool := wtTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE work_tier_observations`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	schema := setupMintTripwires(t, ctx, pool)
	store := NewStore(pool)
	adv := NewAdvisor()
	traffic := representativeTraffic()

	// Phase 1 — ADVISORY ONLY: call the read-only Advisor for every request. It
	// holds no DB handle, so it must add ZERO rows anywhere (not even descriptive).
	for _, r := range traffic {
		got := adv.Advise(PreServeSignals{InputTokens: r.in, ComplexityScore: r.score, PIIDetected: r.pii, GuardrailFired: r.guardrail, LoggingPolicy: r.policy})
		if got.DowngradeEligible != r.wantDowngrade || got.SensitiveOptOut != r.wantSensOptOut {
			t.Errorf("%s: advice downgrade=%v optout=%v, want %v/%v", r.name, got.DowngradeEligible, got.SensitiveOptOut, r.wantDowngrade, r.wantSensOptOut)
		}
		if got.Tier.Complexity != r.wantComplexity || got.Tier.Sensitivity != r.wantSensitivity {
			t.Errorf("%s: advice tier cx=%s sens=%s, want %s/%s", r.name, got.Tier.Complexity, got.Tier.Sensitivity, r.wantComplexity, r.wantSensitivity)
		}
	}
	if n := countRows(t, ctx, pool, "work_tier_observations"); n != 0 {
		t.Fatalf("the Advisor wrote %d rows — it must be read-only", n)
	}
	assertNoMint(t, ctx, pool, schema)

	// Phase 2 — DESCRIPTIVE persist (what the real serve path does post-flush): one
	// descriptive row per request, and STILL not a single mint.
	for _, r := range traffic {
		wt := Classify(r.in, r.out, r.cost, r.score, r.pii, r.guardrail, r.policy)
		if err := store.Record(ctx, "ws-nomint", "feat", r.model, "openai", wt, r.in, r.out, r.cost, r.score, r.pii, r.guardrail); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if n := countRows(t, ctx, pool, "work_tier_observations"); n != len(traffic) {
		t.Errorf("descriptive rows = %d, want %d", n, len(traffic))
	}
	assertNoMint(t, ctx, pool, schema)
}
