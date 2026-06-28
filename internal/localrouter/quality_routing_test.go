package localrouter

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2" // nosemgrep: math-random-used — test RNG only
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// P1 #10 PR-B: quality biases node selection WITHIN the strategy, bounded + exploration-floored, and
// is byte-identical when off. In-memory (no DB) except the sync-loop test.

func qrouter() *Router {
	r := NewRouter(nil) // nil pool: proves SelectEndpoint does ZERO DB reads (proof 6)
	return r
}

func reg(r *Router, id, model string, q float64, n int) {
	r.Register(&LocalEndpoint{ID: id, URL: "http://" + id, Provider: "vllm", Models: []string{model}})
	if n > 0 || q != 0 {
		r.UpdateQuality(id, model, q, n)
	}
}

func seedRand(r *Router, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b9)) // nosemgrep: math-random-used — deterministic test source
	r.SetRand(rng.Float64, rng.IntN)
}

func shares(t *testing.T, r *Router, model string, strat RoutingStrategy, runs int) map[string]int {
	t.Helper()
	out := map[string]int{}
	for i := 0; i < runs; i++ {
		ep, err := r.SelectEndpoint(model, strat)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		out[ep.ID]++
	}
	return out
}

func markHealthy(r *Router) {
	r.mu.Lock()
	for _, e := range r.endpoints {
		e.Healthy = true
	}
	r.mu.Unlock()
}

// (1)+(4) Higher score → larger BOUNDED share; max-q node never 100%; low-q keeps nonzero share.
func TestQuality_HigherScore_LargerButBoundedShare(t *testing.T) {
	r := qrouter()
	reg(r, "hi", "m", 0.9, 50)
	reg(r, "lo", "m", 0.5, 50)
	markHealthy(r)
	r.SetQualityEnabled(func() bool { return true })
	seedRand(r, 1)

	const N = 20000
	s := shares(t, r, "m", StrategyLeastLoaded, N)
	hi, lo := float64(s["hi"])/N, float64(s["lo"])/N
	t.Logf("shares: hi=%.3f lo=%.3f (q 0.9 vs 0.5, LeastLoaded, N=%d)", hi, lo, N)
	if hi <= lo {
		t.Errorf("higher-quality node must win a LARGER share: hi=%.3f lo=%.3f", hi, lo)
	}
	if hi >= 1.0 || hi > 0.95 {
		t.Errorf("max-q node share must be bounded < 1 (ε floor for the other): hi=%.3f", hi)
	}
	if lo <= 0 || lo < 0.05 { // ε=0.15 over 2 nodes ⇒ ≥ ~0.075 from exploration alone
		t.Errorf("low-q node must keep a NONZERO share: lo=%.3f", lo)
	}
}

// (2) A zero-score node (n>0) is never zeroed — keeps ≥ ~ε/n share.
func TestQuality_ZeroScoreNodeStillNonzero(t *testing.T) {
	r := qrouter()
	reg(r, "good", "m", 1.0, 50)
	reg(r, "zero", "m", 0.0, 50) // score 0, but sampled
	markHealthy(r)
	r.SetQualityEnabled(func() bool { return true })
	seedRand(r, 2)
	s := shares(t, r, "m", StrategyLeastLoaded, 20000)
	zero := float64(s["zero"]) / 20000
	t.Logf("zero-score share=%.3f (must be ≥ ~ε/n)", zero)
	if zero < 0.05 {
		t.Errorf("zero-score node must keep ≥ ~ε/n share (exploration floor), got %.3f", zero)
	}
}

// (3) NEUTRAL PRIOR: an unscored node (no row) has q_eff exactly 0.5 and routes like an explicit 0.5.
func TestQuality_UnscoredIsNeutral(t *testing.T) {
	r := qrouter()
	r.Register(&LocalEndpoint{ID: "unscored", Models: []string{"m"}, URL: "http://u"})
	r.mu.RLock()
	q := qEffLocked(r.endpoints[0], "m")
	r.mu.RUnlock()
	if q != 0.5 {
		t.Fatalf("unscored q_eff = %v, want exactly 0.5 (neutral prior, no penalty for new)", q)
	}
	// And it routes ~evenly against an explicit neutral node.
	reg(r, "neutral", "m", 0.5, 100)
	markHealthy(r)
	r.SetQualityEnabled(func() bool { return true })
	seedRand(r, 3)
	s := shares(t, r, "m", StrategyRoundRobin, 20000)
	u, n := float64(s["unscored"])/20000, float64(s["neutral"])/20000
	if d := u - n; d < -0.06 || d > 0.06 {
		t.Errorf("unscored should route ~like neutral: unscored=%.3f neutral=%.3f", u, n)
	}
}

// (5a) FLAG-OFF byte-identical: each strategy returns today's EXACT deterministic pick.
func TestQuality_FlagOff_GoldenPerStrategy(t *testing.T) {
	r := qrouter() // qualityEnabled nil ⇒ off
	for _, id := range []string{"a", "b", "c"} {
		r.Register(&LocalEndpoint{ID: id, Models: []string{"m"}, URL: "http://" + id})
		r.UpdateQuality(id, "m", 1.0, 99) // high quality everywhere — must be IGNORED when off
	}
	markHealthy(r)
	r.mu.Lock()
	atomic.StoreInt64(&r.endpoints[1].activeCount, 0) // b least loaded
	atomic.StoreInt64(&r.endpoints[0].activeCount, 9)
	atomic.StoreInt64(&r.endpoints[2].activeCount, 9)
	r.endpoints[0].AvgLatencyMs, r.endpoints[1].AvgLatencyMs, r.endpoints[2].AvgLatencyMs = 90, 5, 90 // b lowest
	r.endpoints[0].Priority, r.endpoints[1].Priority, r.endpoints[2].Priority = 9, 0, 9               // b top priority
	r.mu.Unlock()

	if ep, _ := r.SelectEndpoint("m", StrategyLeastLoaded); ep.ID != "b" {
		t.Errorf("LeastLoaded off: got %q, want b (deterministic argmin)", ep.ID)
	}
	if ep, _ := r.SelectEndpoint("m", StrategyLowestLatency); ep.ID != "b" {
		t.Errorf("LowestLatency off: got %q, want b", ep.ID)
	}
	if ep, _ := r.SelectEndpoint("m", StrategyPriority); ep.ID != "b" {
		t.Errorf("Priority off: got %q, want b", ep.ID)
	}
	// RoundRobin off: strict cycle independent of quality.
	got := []string{}
	for i := 0; i < 6; i++ {
		ep, _ := r.SelectEndpoint("m", StrategyRoundRobin)
		got = append(got, ep.ID)
	}
	if got[0] == got[1] || got[1] == got[2] {
		t.Errorf("RoundRobin off must cycle distinct endpoints, got %v", got)
	}
}

// (5b) Composes: with quality ON but EQUAL quality, the strategy's base signal still dominates
// (LeastLoaded still favors the unloaded node).
func TestQuality_On_ComposesWithStrategySignal(t *testing.T) {
	r := qrouter()
	reg(r, "loaded", "m", 0.5, 50)
	reg(r, "idle", "m", 0.5, 50) // equal quality
	markHealthy(r)
	r.mu.Lock()
	atomic.StoreInt64(&r.endpoints[0].activeCount, 20) // loaded
	atomic.StoreInt64(&r.endpoints[1].activeCount, 0)  // idle
	r.mu.Unlock()
	r.SetQualityEnabled(func() bool { return true })
	seedRand(r, 5)
	s := shares(t, r, "m", StrategyLeastLoaded, 20000)
	if s["idle"] <= s["loaded"] {
		t.Errorf("equal quality → LeastLoaded base must favor idle: idle=%d loaded=%d", s["idle"], s["loaded"])
	}
}

// (6) NO per-request DB read: SelectEndpoint works with a nil pool + quality ON.
func TestQuality_SelectEndpoint_NoDBRead(t *testing.T) {
	r := qrouter() // pool is nil
	reg(r, "x", "m", 0.8, 10)
	markHealthy(r)
	r.SetQualityEnabled(func() bool { return true })
	seedRand(r, 6)
	if _, err := r.SelectEndpoint("m", StrategyLeastLoaded); err != nil {
		t.Fatalf("SelectEndpoint must not touch the DB (nil pool): %v", err)
	}
}

// (7) NO-LOOP / import-guard: localrouter references no ledger/mint symbol and imports no money pkg.
func TestImportGuard_LocalrouterNoLedgerNoMint(t *testing.T) {
	forbiddenImports := []string{"internal/mining", "internal/poolroyalty", "internal/economy", "internal/povi"}
	forbiddenIdents := map[string]bool{"Credit": true, "CreditTx": true, "CreditHeldTx": true, "MintFromReceipt": true, "LedgerStore": true, "benchmark_node_scores_write": true}
	entries, _ := os.ReadDir(".")
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbiddenImports {
				if strings.Contains(p, bad) {
					t.Errorf("%s imports %q — routing must reach no ledger/mint path", e.Name(), p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbiddenIdents[id.Name] {
				t.Errorf("%s references money symbol %q", e.Name(), id.Name)
			}
			return true
		})
	}
}

// (8) Real-PG: the sync loop loads benchmark_node_scores into the in-memory endpoint quality.
func TestQualitySync_LoadsFromDB_Integration(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG quality-sync test")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_qualitysync_test"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_qualitysync_test CASCADE`,
		`CREATE SCHEMA lens_qualitysync_test`,
		`CREATE TABLE benchmark_node_scores (node_id TEXT NOT NULL, model TEXT NOT NULL, score DOUBLE PRECISION NOT NULL DEFAULT 0,
			sample_count INTEGER NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (node_id, model))`,
		`INSERT INTO benchmark_node_scores (node_id, model, score, sample_count) VALUES ('n1','m',0.83,12)`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	r := NewRouter(pool)
	r.Register(&LocalEndpoint{ID: "n1", Models: []string{"m"}, URL: "http://n1"})
	r.syncQuality(ctx) // one load tick

	r.mu.RLock()
	score, samples := r.endpoints[0].quality["m"], r.endpoints[0].qualitySamples["m"]
	r.mu.RUnlock()
	if score != 0.83 || samples != 12 {
		t.Errorf("after sync: score=%v samples=%v, want 0.83/12", score, samples)
	}
}
