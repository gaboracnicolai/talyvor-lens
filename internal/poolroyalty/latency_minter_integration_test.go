package poolroyalty

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/mining"
)

// Real-PG proofs for proof-of-latency-locality (Proof-of-Improvement instance 3, the LATENCY MINT):
//   - MINT-CORRECTNESS: an exact hand-derived amount (rate × clamp01((median−L)/median) × cohortCost/5).
//   - EXACTLY-ONCE per (node,cohort,model,epoch); a fresh epoch re-pays.
//   - FINAL-EWMA snapshot: a later epoch pays the CURRENT ewma, not the stale one.
//   - C-GATE exact per-(node,model); MIN-NODES; per-candidate unlinked-baseline anti-farm; BATCH CAP;
//     INERT; U6 floor.
// The clock is injected (m.now) so epoch boundaries are deterministic — no sleeps.

var latencyBaseTime = time.Unix(1_000_000_000, 0) // fixed; epoch = Unix()/windowSeconds

func latencyHarness(t *testing.T) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG latency mint test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_latencymint_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_latencymint_test CASCADE`,
		`CREATE SCHEMA lens_latencymint_test`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			held_balance DOUBLE PRECISION NOT NULL DEFAULT 0, lifetime_earned DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_spent DOUBLE PRECISION NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount DOUBLE PRECISION NOT NULL, balance_after DOUBLE PRECISION NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount DOUBLE PRECISION NOT NULL)`,
		`CREATE TABLE inference_nodes (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL)`,
		`CREATE TABLE node_cohort_latency_stats (node_id TEXT NOT NULL, feature_category TEXT NOT NULL,
			input_token_range TEXT NOT NULL, complexity_bucket TEXT NOT NULL, model TEXT NOT NULL,
			latency_ewma DOUBLE PRECISION NOT NULL DEFAULT 0, cost_weight_accum DOUBLE PRECISION NOT NULL DEFAULT 0,
			sample_count BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (node_id, feature_category, input_token_range, complexity_bucket, model))`,
		`CREATE TABLE benchmark_node_scores (node_id TEXT NOT NULL, model TEXT NOT NULL, score DOUBLE PRECISION NOT NULL,
			sample_count INTEGER NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY (node_id, model))`,
		`CREATE TABLE workspace_card_fingerprints (workspace_id TEXT NOT NULL, fingerprint_hash TEXT NOT NULL,
			PRIMARY KEY (workspace_id, fingerprint_hash))`,
		`CREATE TABLE node_latency_mints (request_id TEXT PRIMARY KEY, contributor_workspace_id TEXT NOT NULL,
			latency_skill DOUBLE PRECISION NOT NULL, minted_amount DOUBLE PRECISION NOT NULL, node_id TEXT NOT NULL,
			feature_category TEXT NOT NULL, input_token_range TEXT NOT NULL, complexity_bucket TEXT NOT NULL,
			model TEXT NOT NULL, epoch BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'held',
			finalize_after TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New()) // U6 floor — wired exactly as production
	return pool, ledger
}

// seedNode registers a node under a workspace and (by default) verifies + benchmark-passes it so the only
// variable under test is the latency composition. model defaults handled by the caller's seedLatency.
func seedNode(t *testing.T, pool *pgxpool.Pool, nodeID, ws string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO inference_nodes (id, workspace_id) VALUES ($1,$2)`, nodeID, ws); err != nil {
		t.Fatal(err)
	}
	verifyWorkspace(t, pool, ws)
}

func seedLatency(t *testing.T, pool *pgxpool.Pool, nodeID, model string, latencyEWMA, costAccum float64, samples int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO node_cohort_latency_stats (node_id, feature_category, input_token_range, complexity_bucket, model, latency_ewma, cost_weight_accum, sample_count)
		 VALUES ($1,'chat','small','trivial',$2,$3,$4,$5)`, nodeID, model, latencyEWMA, costAccum, samples); err != nil {
		t.Fatal(err)
	}
}

func seedBenchmark(t *testing.T, pool *pgxpool.Pool, nodeID, model string, score float64, samples int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO benchmark_node_scores (node_id, model, score, sample_count) VALUES ($1,$2,$3,$4)`, nodeID, model, score, samples); err != nil {
		t.Fatal(err)
	}
}

func linkWorkspaces(t *testing.T, pool *pgxpool.Pool, fpHash string, wss ...string) {
	t.Helper()
	for _, ws := range wss {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1,$2) ON CONFLICT DO NOTHING`, ws, fpHash); err != nil {
			t.Fatal(err)
		}
	}
}

func latencyMintRows(t *testing.T, pool *pgxpool.Pool, ws string) (n int, amount, skill float64) {
	t.Helper()
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(sum(minted_amount),0), COALESCE(max(latency_skill),0)
		 FROM node_latency_mints WHERE contributor_workspace_id=$1`, ws).Scan(&n, &amount, &skill)
	return
}

// newLatencyMinterAt builds a minter with an injected fixed clock (deterministic epoch).
func newLatencyMinterAt(pool *pgxpool.Pool, ledger ledgerCreditTx, rate float64, at time.Time) *LatencyMinter {
	m := NewLatencyMinter(pool, ledger, rate, func() bool { return true })
	m.now = func() time.Time { return at }
	return m
}

// (proof 1) MINT-CORRECTNESS — the exact money number. The baseline count EXCLUDES the candidate and needs
// >= 3 distinct unlinked peers (mirrors eval_contribution MinWorkspaces=3), so a winner needs 4+ nodes.
// Cohort (chat/small/trivial/gpt-4o): nodeA L=100 cost=15/5=3.0 ; nodeB=nodeC=nodeD L=300 (all pass+verified).
// nodeA's per-candidate baseline = median(300,300,300) = 300 → margin=(300−100)/300=2/3; costFactor=3.0/5=0.6
// → latency_skill = (2/3)×0.6 = 0.40 → amount = rate(10)×0.40 = 4.00. nodeB/C/D are NOT faster than their own
// baseline (median(100,300,300)=300, 300≮300) ⇒ ONLY nodeA mints, EXACTLY 4.00.
func TestLatencyMint_Correctness_ExactAmount(t *testing.T) {
	pool, ledger := latencyHarness(t)
	ctx := context.Background()
	seedNode(t, pool, "nodeA", "wsA")
	seedBenchmark(t, pool, "nodeA", "gpt-4o", 0.9, 10)
	seedLatency(t, pool, "nodeA", "gpt-4o", 100, 15, 5) // cohort_cost = 15/5 = 3.0
	for _, n := range []struct{ node, ws string }{{"nodeB", "wsB"}, {"nodeC", "wsC"}, {"nodeD", "wsD"}} {
		seedNode(t, pool, n.node, n.ws)
		seedBenchmark(t, pool, n.node, "gpt-4o", 0.9, 10)
		seedLatency(t, pool, n.node, "gpt-4o", 300, 20, 5)
	}

	m := newLatencyMinterAt(pool, ledger, 10, latencyBaseTime)
	n, err := m.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expected exactly ONE mint (only nodeA beats its baseline): n=%d err=%v", n, err)
	}
	rows, amount, skill := latencyMintRows(t, pool, "wsA")
	if rows != 1 || skill < 0.3999 || skill > 0.4001 || amount < 3.999 || amount > 4.001 {
		t.Fatalf("nodeA expected latency_skill 0.40, amount 4.00: rows=%d skill=%.4f amount=%.4f", rows, skill, amount)
	}
	// ATTRIBUTION: the credit lands on the SERVING NODE's workspace (wsA), and nowhere else.
	if _, held := balances(t, pool, "wsA"); held < 3.999 || held > 4.001 {
		t.Fatalf("wsA held_balance %.4f, want 4.00", held)
	}
	for _, ws := range []string{"wsB", "wsC", "wsD"} {
		if n, _, _ := latencyMintRows(t, pool, ws); n != 0 {
			t.Fatalf("%s must not mint (300 ≮ its baseline 300), got %d", ws, n)
		}
	}
}

// (proof 2) EXACTLY-ONCE per epoch + fresh epoch re-pays.
func TestLatencyMint_ExactlyOncePerEpoch_And_NextEpochRepays(t *testing.T) {
	pool, ledger := latencyHarness(t)
	ctx := context.Background()
	seedWinnerCohort(t, pool, 100, 300)

	m := newLatencyMinterAt(pool, ledger, 10, latencyBaseTime)
	if n, _ := m.RunOnce(ctx); n != 1 {
		t.Fatalf("epoch E first sweep: want 1 mint, got %d", n)
	}
	if n, _ := m.RunOnce(ctx); n != 0 {
		t.Fatalf("epoch E second sweep: want 0 (ON CONFLICT), got %d", n)
	}
	// Advance one full window → a NEW epoch → the SAME node re-pays (rolling reward for sustained speed).
	m.now = func() time.Time { return latencyBaseTime.Add(time.Duration(m.windowSeconds) * time.Second) }
	if n, _ := m.RunOnce(ctx); n != 1 {
		t.Fatalf("next epoch: want a fresh mint, got %d", n)
	}
	if rows, _, _ := latencyMintRows(t, pool, "wsA"); rows != 2 {
		t.Fatalf("wsA want 2 mints across 2 epochs, got %d", rows)
	}
}

// (proof 3) FINAL-EWMA snapshot: a later epoch pays the CURRENT ewma, not the stale first-tick value.
func TestLatencyMint_FinalEWMASnapshot(t *testing.T) {
	pool, ledger := latencyHarness(t)
	ctx := context.Background()
	seedWinnerCohort(t, pool, 100, 300) // nodeA=100, baseline 300 → skill 0.40 → amount 4.00

	m := newLatencyMinterAt(pool, ledger, 10, latencyBaseTime)
	if n, _ := m.RunOnce(ctx); n != 1 {
		t.Fatalf("epoch E: want 1, got %d", n)
	}
	// nodeA gets FASTER (100→50): baseline still 300 → margin (300−50)/300=5/6 → skill (5/6)×0.6=0.50 → 5.00.
	if _, err := pool.Exec(ctx, `UPDATE node_cohort_latency_stats SET latency_ewma=50 WHERE node_id='nodeA'`); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return latencyBaseTime.Add(time.Duration(m.windowSeconds) * time.Second) }
	if n, _ := m.RunOnce(ctx); n != 1 {
		t.Fatalf("next epoch: want 1, got %d", n)
	}
	// The MOST RECENT mint must reflect the UPDATED ewma (5.00), not the stale 4.00.
	var latest float64
	if err := pool.QueryRow(ctx, `SELECT minted_amount FROM node_latency_mints WHERE contributor_workspace_id='wsA' ORDER BY created_at DESC, epoch DESC LIMIT 1`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if latest < 4.999 || latest > 5.001 {
		t.Fatalf("final-EWMA snapshot: latest mint %.4f, want 5.00 (tracks current ewma 50, not stale 100)", latest)
	}
}

// (proof 4) C-GATE exact per-(node,model): nodeX is GREAT on gpt-4o (0.9) but POOR on claude (0.3). Serving
// the claude cohort fast ⇒ ZERO pay for claude (the join is on (node_id, model), not a per-node average).
func TestLatencyMint_CGate_ExactPerNodeModel(t *testing.T) {
	pool, ledger := latencyHarness(t)
	ctx := context.Background()
	// nodeX fastest in the claude cohort with 3 unlinked peers (min-nodes PASSES), but its claude benchmark is
	// BELOW threshold — so the ONLY thing stopping nodeX is the exact per-(node,model) C-gate.
	seedNode(t, pool, "nodeX", "wsX")
	seedBenchmark(t, pool, "nodeX", "gpt-4o", 0.9, 10) // great on a DIFFERENT model
	seedBenchmark(t, pool, "nodeX", "claude", 0.3, 10) // POOR on the model it's serving fast
	seedLatency(t, pool, "nodeX", "claude", 50, 20, 5) // fastest by far
	for _, p := range []struct {
		node, ws string
		lat      float64
	}{{"nodeP", "wsP", 200}, {"nodeQ", "wsQ", 300}, {"nodeR", "wsR", 400}} {
		seedNode(t, pool, p.node, p.ws)
		seedBenchmark(t, pool, p.node, "claude", 0.9, 10)
		seedLatency(t, pool, p.node, "claude", p.lat, 20, 5)
	}

	m := newLatencyMinterAt(pool, ledger, 10, latencyBaseTime)
	if _, err := m.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := latencyMintRows(t, pool, "wsX"); n != 0 {
		t.Fatalf("nodeX must mint 0 for claude (claude score 0.3 < 0.7) despite gpt-4o=0.9 — the gate is per-(node,model), got %d", n)
	}
	// The cohort IS otherwise live (min-nodes satisfied): a well-scored peer mints — proving nodeX's zero is
	// the C-gate, not a dead cohort.
	var totalMints int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM node_latency_mints`).Scan(&totalMints); err != nil {
		t.Fatal(err)
	}
	if totalMints == 0 {
		t.Fatal("expected a well-scored peer to mint (cohort is live) — nodeX's zero must be the C-gate, not min-nodes")
	}
}

// (proof 5) MIN-NODES: a cohort with < 3 unlinked peers pays nothing.
func TestLatencyMint_MinUnlinkedNodes(t *testing.T) {
	pool, ledger := latencyHarness(t)
	ctx := context.Background()
	// Only TWO nodes total ⇒ a candidate has at most 1 unlinked peer ⇒ n_unlinked < 3 ⇒ no pay.
	for _, n := range []struct{ node, ws string }{{"n1", "w1"}, {"n2", "w2"}} {
		seedNode(t, pool, n.node, n.ws)
		seedBenchmark(t, pool, n.node, "gpt-4o", 0.9, 10)
	}
	seedLatency(t, pool, "n1", "gpt-4o", 50, 20, 5)
	seedLatency(t, pool, "n2", "gpt-4o", 300, 20, 5)
	m := newLatencyMinterAt(pool, ledger, 10, latencyBaseTime)
	if n, err := m.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("cohort under MinUnlinkedNodes must mint 0: n=%d err=%v", n, err)
	}
}

// (proof 6) BASELINE ANTI-FARM: a candidate's fingerprint-LINKED slow nodes do NOT count toward its baseline.
// nodeA (fast) + 3 LINKED slow sockpuppets. If the linked nodes counted, nodeA's baseline would be ~1000 and
// it would trivially mint; because they're EXCLUDED, nodeA has 0 UNLINKED peers ⇒ no pay.
func TestLatencyMint_BaselineExcludesLinkedSockpuppets(t *testing.T) {
	pool, ledger := latencyHarness(t)
	ctx := context.Background()
	seedNode(t, pool, "nodeA", "wsA")
	seedBenchmark(t, pool, "nodeA", "gpt-4o", 0.9, 10)
	seedLatency(t, pool, "nodeA", "gpt-4o", 100, 20, 5)
	// three slow sockpuppets, all fingerprint-linked to wsA (shared card) — should be excluded from wsA's baseline.
	linkWorkspaces(t, pool, "sharedcard", "wsA", "wsS1", "wsS2", "wsS3")
	for i, ws := range []string{"wsS1", "wsS2", "wsS3"} {
		node := "sock" + string(rune('1'+i))
		seedNode(t, pool, node, ws)
		seedBenchmark(t, pool, node, "gpt-4o", 0.9, 10)
		seedLatency(t, pool, node, "gpt-4o", 1000, 20, 5) // deliberately slow to inflate a naive baseline
	}
	m := newLatencyMinterAt(pool, ledger, 10, latencyBaseTime)
	if n, err := m.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("linked slow sockpuppets must NOT count toward wsA's baseline ⇒ 0 unlinked peers ⇒ 0 mint: n=%d err=%v", n, err)
	}
}

// (proof 7) BATCH CAP: with more eligible candidates than the cap, one RunOnce processes at most the cap.
func TestLatencyMint_BatchCap_Bounds(t *testing.T) {
	pool, ledger := latencyHarness(t)
	ctx := context.Background()
	// 5 nodes at latencies {100,100,100,300,300}: each 100-node beats its baseline (median of the other four
	// = median(100,100,300,300)=200) ⇒ THREE eligible winners.
	lat := []float64{100, 100, 100, 300, 300}
	for i, l := range lat {
		node, ws := "bn"+string(rune('1'+i)), "bw"+string(rune('1'+i))
		seedNode(t, pool, node, ws)
		seedBenchmark(t, pool, node, "gpt-4o", 0.9, 10)
		seedLatency(t, pool, node, "gpt-4o", l, 20, 5)
	}
	m := newLatencyMinterAt(pool, ledger, 10, latencyBaseTime)
	m.batchSize = 2 // cap below the 3 eligible winners

	n, err := m.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n > 2 {
		t.Fatalf("batch cap 2 must bound one RunOnce to <= 2 mints, got %d", n)
	}
	// Progress: subsequent capped sweeps eventually mint all three winners (the cap bounds per-tick, not total).
	for i := 0; i < 3; i++ {
		if _, err := m.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM node_latency_mints`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("across capped sweeps all 3 winners mint exactly once: got %d", total)
	}
}

// (proof 8) INERT: rate-0 ⇒ zero mints + zero rows; flag-off ⇒ RunOnce no-op.
func TestLatencyMint_Inert(t *testing.T) {
	pool, ledger := latencyHarness(t)
	ctx := context.Background()
	seedWinnerCohort(t, pool, 100, 300)

	// rate-0 ⇒ nil anchor ⇒ no mint even with the flag on.
	inert := newLatencyMinterAt(pool, ledger, 0, latencyBaseTime)
	if n, err := inert.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("rate-0 must mint 0: n=%d err=%v", n, err)
	}
	if n, _, _ := latencyMintRows(t, pool, "wsA"); n != 0 {
		t.Fatalf("rate-0 must write zero node_latency_mints rows, got %d", n)
	}
	// flag-off ⇒ RunOnce no-op even with a positive rate.
	off := NewLatencyMinter(pool, ledger, 10, func() bool { return false })
	off.now = func() time.Time { return latencyBaseTime }
	if n, err := off.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("flag-off must no-op: n=%d err=%v", n, err)
	}
}

// (proof 10) U6 FLOOR: an UNVERIFIED node-workspace mints nothing (verifyEarn rolls back); verifying then
// lets the SAME state mint, and the credit type is TypeLatencyLocalityHeld (∈ mintTypeList ⇒ U6 + 24h cap).
func TestLatencyMint_U6Floor(t *testing.T) {
	pool, ledger := latencyHarness(t)
	ctx := context.Background()
	// Build a cohort (4 nodes so nodeA clears MinUnlinkedNodes) where nodeA would mint, but nodeA's workspace
	// is NOT verified.
	if _, err := pool.Exec(ctx, `INSERT INTO inference_nodes (id, workspace_id) VALUES ('nodeA','wsUnv')`); err != nil {
		t.Fatal(err)
	}
	seedBenchmark(t, pool, "nodeA", "gpt-4o", 0.9, 10)
	seedLatency(t, pool, "nodeA", "gpt-4o", 100, 15, 5)
	for _, n := range []struct{ node, ws string }{{"nodeB", "wsB"}, {"nodeC", "wsC"}, {"nodeD", "wsD"}} {
		seedNode(t, pool, n.node, n.ws)
		seedBenchmark(t, pool, n.node, "gpt-4o", 0.9, 10)
		seedLatency(t, pool, n.node, "gpt-4o", 300, 20, 5)
	}

	m := newLatencyMinterAt(pool, ledger, 10, latencyBaseTime)
	if n, err := m.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("unverified node-workspace must mint 0 (U6 rolls back): n=%d err=%v", n, err)
	}
	if n, _, _ := latencyMintRows(t, pool, "wsUnv"); n != 0 {
		t.Fatalf("U6: the claim row must be rolled back, got %d", n)
	}
	// Verify → the SAME state now mints.
	verifyWorkspace(t, pool, "wsUnv")
	if n, err := m.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("verified node-workspace must now mint 1: n=%d err=%v", n, err)
	}
	var typ string
	if err := pool.QueryRow(ctx, `SELECT type FROM lens_token_ledger WHERE workspace_id='wsUnv' ORDER BY created_at DESC LIMIT 1`).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != mining.TypeLatencyLocalityHeld {
		t.Fatalf("latency mint ledger type = %q, want %q (∈ mintTypeList ⇒ U6 floor + 24h cap)", typ, mining.TypeLatencyLocalityHeld)
	}
}

// seedWinnerCohort is the canonical (chat/small/trivial/gpt-4o) cohort: nodeA/wsA fastest at `la` (mints),
// plus THREE slower unlinked peers all at `lslow` so nodeA clears MinUnlinkedNodes (>= 3 distinct unlinked
// PEERS, candidate excluded). All benchmark-pass + verified. cohort_cost = 15/5 = 3.0 (costFactor 0.6).
// With la=100, lslow=300: nodeA baseline=median(300,300,300)=300 → skill=(2/3)×0.6=0.40 → amount 4.00; the
// peers never beat their own baseline (median(100,300,300)=300, 300≮300) ⇒ only nodeA mints.
func seedWinnerCohort(t *testing.T, pool *pgxpool.Pool, la, lslow float64) {
	t.Helper()
	seedNode(t, pool, "nodeA", "wsA")
	seedBenchmark(t, pool, "nodeA", "gpt-4o", 0.9, 10)
	seedLatency(t, pool, "nodeA", "gpt-4o", la, 15, 5)
	for _, n := range []struct{ node, ws string }{{"nodeB", "wsB"}, {"nodeC", "wsC"}, {"nodeD", "wsD"}} {
		seedNode(t, pool, n.node, n.ws)
		seedBenchmark(t, pool, n.node, "gpt-4o", 0.9, 10)
		seedLatency(t, pool, n.node, "gpt-4o", lslow, 15, 5)
	}
}
