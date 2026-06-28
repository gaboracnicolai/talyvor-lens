package benchprobe_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/benchprobe"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/povi"
)

// PR-A.5 money proof: a probe delivered through REAL /inference to a staked + earn_verified node with
// PoVI minting ON is RECORDED but mints ZERO — the gateway-side request_id suppression. Plus the
// suppression-only contrast (a non-probe receipt mints as today) and the on-the-wire node-blind +
// happens-before checks. Wired like prod: SetMintVerifier(earnverify.New()) + minting ON +
// SetProbeChecker(benchStore.IsProbe).

func suppressionHarness(t *testing.T) (*pgxpool.Pool, *mining.LedgerStore, *mining.ComputeMiner, *benchprobe.Store) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG suppression test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_benchsuppress_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_benchsuppress_test CASCADE`,
		`CREATE SCHEMA lens_benchsuppress_test`,
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount DOUBLE PRECISION NOT NULL)`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			locked_balance DOUBLE PRECISION NOT NULL DEFAULT 0, held_balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_earned DOUBLE PRECISION NOT NULL DEFAULT 0, lifetime_spent DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount DOUBLE PRECISION NOT NULL, balance_after DOUBLE PRECISION NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE inference_nodes (id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text, workspace_id TEXT NOT NULL,
			url TEXT NOT NULL, provider TEXT NOT NULL, models TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
			gpu_type TEXT NOT NULL DEFAULT 'cpu', max_concurrent INTEGER NOT NULL DEFAULT 1,
			price_per_token DOUBLE PRECISION NOT NULL DEFAULT 0.050, active BOOLEAN NOT NULL DEFAULT TRUE,
			verified BOOLEAN NOT NULL DEFAULT FALSE, ed25519_pubkey TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE node_metrics (node_id TEXT PRIMARY KEY REFERENCES inference_nodes(id) ON DELETE CASCADE,
			requests_served INTEGER NOT NULL DEFAULT 0, tokens_served BIGINT NOT NULL DEFAULT 0,
			avg_latency_ms BIGINT NOT NULL DEFAULT 0, error_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
			uptime_pct DOUBLE PRECISION NOT NULL DEFAULT 100, last_active_at TIMESTAMPTZ)`,
		`CREATE TABLE povi_stakes (node_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, amount DOUBLE PRECISION NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'active', slashed_amount DOUBLE PRECISION NOT NULL DEFAULT 0,
			locked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), unbond_at TIMESTAMPTZ, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE povi_receipts (request_id TEXT PRIMARY KEY, node_id TEXT NOT NULL, workspace_id TEXT NOT NULL,
			model TEXT NOT NULL, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			merkle_root TEXT NOT NULL, verified BOOLEAN NOT NULL, timestamp BIGINT NOT NULL,
			leaf_count INTEGER NOT NULL DEFAULT 0, leaf_kind TEXT NOT NULL DEFAULT 'rune', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE benchmark_eval_items (id TEXT PRIMARY KEY, input TEXT NOT NULL, expected_output TEXT NOT NULL,
			eval_method TEXT NOT NULL DEFAULT 'exact', pass_threshold DOUBLE PRECISION NOT NULL DEFAULT 1.0,
			active BOOLEAN NOT NULL DEFAULT TRUE, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), retired_at TIMESTAMPTZ)`,
		`CREATE TABLE benchmark_node_scores (node_id TEXT NOT NULL, model TEXT NOT NULL, score DOUBLE PRECISION NOT NULL DEFAULT 0,
			sample_count INTEGER NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (node_id, model))`,
		`CREATE TABLE benchmark_probes (id TEXT PRIMARY KEY, node_id TEXT NOT NULL, item_id TEXT NOT NULL, request_id TEXT,
			served_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), score DOUBLE PRECISION NOT NULL DEFAULT 0, UNIQUE (node_id, item_id))`,
		`CREATE INDEX idx_benchmark_probes_request ON benchmark_probes (request_id)`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New())
	return pool, ledger, mining.NewComputeMiner(ledger, pool), benchprobe.NewStore(pool)
}

func mintRows(t *testing.T, pool *pgxpool.Pool, ws string) int {
	t.Helper()
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM lens_token_ledger WHERE workspace_id=$1 AND type='receipt_mine_provisional'`, ws).Scan(&n)
	return n
}

// registerStakedVerifiedNode: register a node pointed at nodeURL, stake 100, vouch earn_verified —
// so it WOULD mint a receipt (isolating the suppression as the cause of zero-mint).
func registerStakedVerifiedNode(t *testing.T, pool *pgxpool.Pool, ledger *mining.LedgerStore, miner *mining.ComputeMiner, ws, nodeURL string) (nodeID string, priv ed25519.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	pub, p, _ := ed25519.GenerateKey(rand.Reader)
	priv = p
	node, err := miner.RegisterNode(ctx, mining.InferenceNode{
		WorkspaceID: ws, URL: nodeURL, Provider: "vllm", GPUType: "cpu",
		Models: []string{"trial-mock"}, Ed25519PubKey: povi.EncodePublicKey(pub),
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	nodeID = node.ID
	if err := ledger.Credit(ctx, ws, 150, "closed_test_grant", "bootstrap", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	sm := povi.NewStakeManager(povi.NewNodeStakeStore(pool), ledger, miner.NodeWorkspace, 100, 0, pool)
	if _, err := sm.Stake(ctx, nodeID, 100); err != nil {
		t.Fatalf("Stake: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, earn_verified) VALUES ($1,true) ON CONFLICT (id) DO UPDATE SET earn_verified=true`, ws); err != nil {
		t.Fatal(err)
	}
	return nodeID, priv
}

func newProcessor(pool *pgxpool.Pool, ledger *mining.LedgerStore, miner *mining.ComputeMiner, bench *benchprobe.Store, mintingOn bool) *povi.Processor {
	sm := povi.NewStakeManager(povi.NewNodeStakeStore(pool), ledger, miner.NodeWorkspace, 100, 0, pool)
	lookup := func(ctx context.Context, nodeID string) (ed25519.PublicKey, error) {
		enc, err := miner.NodePubKey(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		return povi.DecodePublicKey(enc)
	}
	proc := povi.NewProcessor(povi.NewStore(pool), ledger, lookup, sm.IsEligible, mintingOn)
	if bench != nil {
		proc.SetProbeChecker(bench.IsProbe) // P1 #10 suppression
	}
	return proc
}

// (4)+(B)+(C) Honest probe through REAL /inference, minting ON → receipt RECORDED, mints ZERO.
func TestProbe_RecordsButMintsZero_MintingOn_Integration(t *testing.T) {
	pool, ledger, miner, bench := suppressionHarness(t)
	ctx := context.Background()
	const ws, model = "node-ws", "trial-mock"

	// httptest node: asserts the probe row is ALREADY committed (happens-before), captures the wire
	// body (node-blind), returns an answer.
	var gotBody []byte
	var gotReqID string
	probeRowExisted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReqID = r.Header.Get("X-Request-ID")
		gotBody, _ = io.ReadAll(r.Body)
		ex, _ := bench.IsProbe(r.Context(), gotReqID) // (B) the suppression key must already exist
		probeRowExisted = ex
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"4"}`))
	}))
	defer srv.Close()

	nodeID, nodePriv := registerStakedVerifiedNode(t, pool, ledger, miner, ws, srv.URL)
	if err := bench.SeedItem(ctx, benchprobe.EvalItem{ID: "i1", Input: "What is 2+2?", ExpectedOutput: "4", EvalMethod: "exact"}); err != nil {
		t.Fatal(err)
	}

	// Drive the probe through the REAL HTTPDelivery (gateway-signed node-auth token, #242).
	_, gwPriv, _ := ed25519.GenerateKey(rand.Reader)
	signer := func(nID, reqID, bodyHash string, exp int64) (string, error) {
		return povi.SignNodeAuthToken(gwPriv, nID, reqID, bodyHash, exp)
	}
	delivery := benchprobe.NewHTTPDelivery(signer, miner.NodeURL, srv.Client())
	sched := benchprobe.NewScheduler(bench, delivery, func() bool { return true })
	if err := sched.RunOnceForNode(ctx, nodeID, model); err != nil {
		t.Fatalf("RunOnceForNode: %v", err)
	}

	// (B) happens-before: the probe row existed when the node was called.
	if !probeRowExisted {
		t.Error("happens-before VIOLATED: benchmark_probes row not committed before the probe was issued")
	}
	// (C) node-blind: the on-the-wire body has the input but NOT the ground truth.
	if !strings.Contains(string(gotBody), "What is 2+2?") || strings.Contains(string(gotBody), "expected") {
		t.Errorf("node-blind VIOLATED: wire body = %s", gotBody)
	}

	// Now the (honest) node submits the receipt it signed for the probe, echoing X-Request-ID.
	rec := povi.SignReceipt(nodePriv, povi.Receipt{
		RequestID: gotReqID, NodeID: nodeID, WorkspaceID: ws, Model: model,
		InputTokens: 10, OutputTokens: 10, Timestamp: 1700000000, LeafCount: 10,
	})
	res, err := newProcessor(pool, ledger, miner, bench, true).Process(ctx, rec) // minting ON + suppression
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	// (4) RECORDED (audit) but MINTS ZERO.
	var recorded bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM povi_receipts WHERE request_id=$1)`, gotReqID).Scan(&recorded)
	if !recorded {
		t.Error("probe receipt must be RECORDED for audit")
	}
	if res.Minted {
		t.Errorf("probe MUST NOT mint; res=%+v", res)
	}
	if n := mintRows(t, pool, ws); n != 0 {
		t.Errorf("probe produced %d receipt_mine_provisional rows, want 0", n)
	}
	if !strings.Contains(res.Reason, "probe") {
		t.Errorf("reason %q should note the probe suppression", res.Reason)
	}
}

// (E) Suppression-ONLY: a NON-probe receipt (request_id not in benchmark_probes) mints exactly as
// today — minting ON, staked + verified node. The guard changes nothing for real earning traffic.
func TestNonProbeReceipt_MintsAsToday_Integration(t *testing.T) {
	pool, ledger, miner, bench := suppressionHarness(t)
	ctx := context.Background()
	const ws, model = "node-ws", "trial-mock"
	nodeID, nodePriv := registerStakedVerifiedNode(t, pool, ledger, miner, ws, "http://unused")

	rec := povi.SignReceipt(nodePriv, povi.Receipt{
		RequestID: "real-earning-req-1", NodeID: nodeID, WorkspaceID: ws, Model: model, // NOT in benchmark_probes
		InputTokens: 10, OutputTokens: 10, Timestamp: 1700000000, LeafCount: 10,
	})
	res, err := newProcessor(pool, ledger, miner, bench, true).Process(ctx, rec) // suppression wired, but not a probe
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Minted || res.Amount <= 0 {
		t.Fatalf("non-probe receipt must mint as today; res=%+v", res)
	}
	if n := mintRows(t, pool, ws); n != 1 {
		t.Errorf("non-probe mint rows %d, want 1 (suppression must not touch real earning)", n)
	}
}
