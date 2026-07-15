package routingbrain

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/keel"
	"github.com/talyvor/lens/internal/modelcapability"
)

// mintTripwireTables — the canonical economy sinks. A FULL brain cycle (offline
// compute + upsert, serving advisory + autonomous resolve) must leave every one at
// zero rows. The brain reads outcomes and advises routing; it never mints.
var mintTripwireTables = []string{
	"lens_token_balances", "token_events", "traffic_mint_holds", "held_mint_adjudications", "lxc_ledger",
}

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

// TestBrain_FullCycle_NoMint_RealPG — the headline mint-free proof. Run the WHOLE
// brain end-to-end on real PG — offline compute+upsert, workspace autonomous opt-in,
// serving-side Refresh, and an advisory AND an autonomous Resolve — then assert that
// EVERY canonical mint/ledger table has zero rows. The only rows written are the
// descriptive recommendations.
func TestBrain_FullCycle_NoMint_RealPG(t *testing.T) {
	pool := rbTestPool(t)
	ctx := context.Background()
	seedH2AndKeel(t, ctx, pool)
	schema := setupMintTripwires(t, ctx, pool)

	store := NewStore(pool)
	cost := func(m string) float64 { // A/B cheap, everything else pricey
		if m == "expensive-safe" {
			return 100.0
		}
		return 1.0
	}
	// OFFLINE: compute + upsert for two workspaces (one will go autonomous).
	job := NewJob(modelcapability.NewStore(pool), keel.NewReader(pool),
		fakeWorkspaces{ws: []WorkspaceModels{
			{WorkspaceID: "wsAdv", AllowedModels: []string{"A", "B"}},
			{WorkspaceID: "wsAuto", AllowedModels: []string{"A", "B"}},
		}}, store, cost)
	if err := job.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if err := store.SetAutonomous(ctx, "wsAuto"); err != nil {
		t.Fatalf("SetAutonomous: %v", err)
	}

	// SERVING: refresh the in-memory brain and resolve both postures.
	brain := New(store, cost, Config{Enabled: true})
	if err := brain.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// Advisory: route stays the safe model.
	if d, ok := brain.Resolve("wsAdv", 0, "router-safe", []string{"A", "B", "router-safe"}); !ok || d.Model != "router-safe" || d.Applied {
		t.Errorf("advisory resolve wrong: %+v ok=%v", d, ok)
	}
	// Autonomous: applies the brain's verified pick over an expensive safe model.
	if d, ok := brain.Resolve("wsAuto", 0, "expensive-safe", []string{"A", "B", "expensive-safe"}); !ok || !d.Applied {
		t.Errorf("autonomous resolve should apply the brain pick: %+v ok=%v", d, ok)
	}

	// The descriptive write happened...
	recs, err := store.LoadRecommendations(ctx)
	if err != nil || len(recs) == 0 {
		t.Fatalf("expected descriptive recommendations; got %d err=%v", len(recs), err)
	}
	// ...and NOTHING minted.
	for _, tbl := range mintTripwireTables {
		var n int
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.%s`, schema, tbl)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("mint/ledger table %s has %d rows — the brain must never mint", tbl, n)
		}
	}
}
