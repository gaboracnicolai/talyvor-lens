package povi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/povi"
)

// PoVI node closed-test harness (real PG): the validator-earning RECEIPT path end-to-end, with
// real-money mint OFF. register → grant starting LENS via the SANCTIONED ledger.Credit → stake via
// the StakeManager (the body of POST /v1/povi/nodes/{id}/stake) → vouch (earn_verified) → sign a
// receipt with the node key → Process. Components are wired EXACTLY as production (main.go:745-765):
// the verifier-gated ledger, ComputeMiner.RegisterNode/NodePubKey, NewStakeManager over the same
// ledger lock primitive, and NewProcessor with the same poviLookup/stakeEligible adapters.
// POVIMintingEnabled is never flipped as a deploy default — minting=true is constructed IN-PROCESS
// only, in the would-mint contrast test. Private schema (no shared-DB clobber). Gated on LENS_TEST_DATABASE_URL.

const poviHarnessSchema = "lens_povi_node_harness"

type nodeHarness struct {
	pool     *pgxpool.Pool
	ledger   *mining.LedgerStore
	miner    *mining.ComputeMiner
	stakeMgr *povi.StakeManager
	lookup   povi.PubKeyLookup
}

func poviNodeHarness(t *testing.T) *nodeHarness {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG PoVI node harness test")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = poviHarnessSchema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS ` + poviHarnessSchema + ` CASCADE`,
		`CREATE SCHEMA ` + poviHarnessSchema,
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL)`, // BIGINT µLXC (prod 0083)
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			locked_balance BIGINT NOT NULL DEFAULT 0, held_balance BIGINT NOT NULL DEFAULT 0,
			lifetime_earned BIGINT NOT NULL DEFAULT 0, lifetime_spent BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`, // BIGINT µLENS (prod 0082)
		`CREATE TABLE lens_token_ledger (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL, description TEXT,
			metadata JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE inference_nodes (id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text, workspace_id TEXT NOT NULL,
			url TEXT NOT NULL, provider TEXT NOT NULL, models TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],
			gpu_type TEXT NOT NULL DEFAULT 'cpu', max_concurrent INTEGER NOT NULL DEFAULT 1,
			price_per_token DOUBLE PRECISION NOT NULL DEFAULT 0.050, active BOOLEAN NOT NULL DEFAULT TRUE,
			verified BOOLEAN NOT NULL DEFAULT FALSE, ed25519_pubkey TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE node_metrics (node_id TEXT PRIMARY KEY REFERENCES inference_nodes(id) ON DELETE CASCADE,
			requests_served INTEGER NOT NULL DEFAULT 0, tokens_served BIGINT NOT NULL DEFAULT 0,
			avg_latency_ms BIGINT NOT NULL DEFAULT 0, error_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
			uptime_pct DOUBLE PRECISION NOT NULL DEFAULT 100, last_active_at TIMESTAMPTZ)`,
		`CREATE TABLE povi_stakes (node_id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, amount BIGINT NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'active', slashed_amount BIGINT NOT NULL DEFAULT 0,
			locked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), unbond_at TIMESTAMPTZ, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`, // BIGINT µLENS (prod 0082)
		`CREATE TABLE povi_receipts (request_id TEXT PRIMARY KEY, node_id TEXT NOT NULL, workspace_id TEXT NOT NULL,
			model TEXT NOT NULL, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			merkle_root TEXT NOT NULL, verified BOOLEAN NOT NULL, timestamp BIGINT NOT NULL,
			leaf_count INTEGER NOT NULL DEFAULT 0, leaf_kind TEXT NOT NULL DEFAULT 'rune',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE served_request_measurements (request_id TEXT PRIMARY KEY, node_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL, output_tokens INTEGER NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`, // 0099 (gateway mint-basis)
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New()) // U6 floor — wired unconditionally, as production
	miner := mining.NewComputeMiner(ledger, pool)
	stakeMgr := povi.NewStakeManager(povi.NewNodeStakeStore(pool), ledger, miner.NodeWorkspace, 100, 0, pool)
	lookup := povi.PubKeyLookup(func(lookupCtx context.Context, nodeID string) (ed25519.PublicKey, error) {
		enc, err := miner.NodePubKey(lookupCtx, nodeID)
		if err != nil {
			return nil, err
		}
		return povi.DecodePublicKey(enc)
	})
	return &nodeHarness{pool: pool, ledger: ledger, miner: miner, stakeMgr: stakeMgr, lookup: lookup}
}

// registerNode runs the REAL RegisterNode path, returning the nodeID + its keypair.
func (h *nodeHarness) registerNode(t *testing.T, ownerWS string) (string, ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node, err := h.miner.RegisterNode(context.Background(), mining.InferenceNode{
		WorkspaceID: ownerWS, URL: "http://node:9000", Provider: "vllm", GPUType: "cpu",
		Models: []string{"test-model"}, Ed25519PubKey: povi.EncodePublicKey(pub),
	})
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	return node.ID, priv, pub
}

func (h *nodeHarness) vouch(t *testing.T, ws string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, earn_verified) VALUES ($1, true) ON CONFLICT (id) DO UPDATE SET earn_verified = true`, ws); err != nil {
		t.Fatalf("vouch: %v", err)
	}
}

// bootstrapStake grants starting LENS via the SANCTIONED public ledger.Credit (a non-mint
// 'closed_test_grant' type so the grant itself isn't earn-floor-gated — a grant is not an earning),
// then stakes via the StakeManager (the money body of POST /v1/povi/nodes/{id}/stake). Never writes
// povi_stakes/locked_balance directly.
func (h *nodeHarness) bootstrapStake(t *testing.T, ctx context.Context, ws, nodeID string) {
	t.Helper()
	if err := h.ledger.Credit(ctx, ws, 150, "closed_test_grant", "PoVI closed-test stake bootstrap", map[string]interface{}{"node_id": nodeID}); err != nil {
		t.Fatalf("Credit (sanctioned grant): %v", err)
	}
	if _, err := h.stakeMgr.Stake(ctx, nodeID, 100); err != nil {
		t.Fatalf("Stake (via StakeManager): %v", err)
	}
}

// recordMeasurement writes Lens's OWN gateway measurement for a served request
// (migration 0099) — the mint basis the receipt→LENS mint prices on and binds to
// the serving node. In production proxy.tryNodeRouting writes this at dispatch.
func (h *nodeHarness) recordMeasurement(t *testing.T, ctx context.Context, requestID, nodeID, ws string, outputTokens int) {
	t.Helper()
	if err := povi.NewMeasurementStore(h.pool).Record(ctx, requestID, nodeID, ws, outputTokens); err != nil {
		t.Fatalf("recordMeasurement: %v", err)
	}
}

func signedReceipt(priv ed25519.PrivateKey, nodeID, ws string) povi.Receipt {
	return povi.SignReceipt(priv, povi.Receipt{
		RequestID: "req-" + nodeID, NodeID: nodeID, WorkspaceID: ws, Model: "test-model",
		InputTokens: 100, OutputTokens: 50, Timestamp: 1700000000, LeafCount: 50,
	})
}

// (1) The closed-test guarantee: a VERIFIABLE receipt flows end-to-end with minting OFF.
func TestPoVINode_VerifiedNonMintingReceipt_Integration(t *testing.T) {
	h := poviNodeHarness(t)
	ctx := context.Background()
	ws := "node-owner-ws"

	nodeID, priv, _ := h.registerNode(t, ws)
	h.vouch(t, ws)
	h.bootstrapStake(t, ctx, ws, nodeID)
	if !h.stakeMgr.IsEligible(ctx, nodeID) {
		t.Fatal("node must be stake-eligible after staking 100 >= min 100")
	}

	// Process with minting OFF (the deploy default; never flipped here).
	proc := povi.NewProcessor(povi.NewStore(h.pool), h.ledger, h.lookup, h.stakeMgr.IsEligible, false)
	res, err := proc.Process(ctx, signedReceipt(priv, nodeID, ws))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	// "verifiable receipt flowed" = ed25519-verified against the registered key, stake-eligible,
	// recorded — and ZERO LENS minted under the default flag.
	if !res.Verified {
		t.Errorf("Verified=%v, want true (ed25519 sig valid vs the registered node key)", res.Verified)
	}
	if !res.StakeEligible {
		t.Errorf("StakeEligible=%v, want true (staked via the API)", res.StakeEligible)
	}
	if res.Minted || res.Amount != 0 {
		t.Errorf("Minted=%v Amount=%v, want false/0 (POVIMinting OFF)", res.Minted, res.Amount)
	}
	var verified bool
	if err := h.pool.QueryRow(ctx, `SELECT verified FROM povi_receipts WHERE request_id=$1`, "req-"+nodeID).Scan(&verified); err != nil {
		t.Fatalf("receipt row not recorded: %v", err)
	}
	if !verified {
		t.Error("povi_receipts row must be recorded with verified=true")
	}
	// Money guard: no LENS minted to the node workspace beyond the bootstrap grant (150) minus the stake lock (100).
	var bal, locked float64
	_ = h.pool.QueryRow(ctx, `SELECT balance, locked_balance FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&bal, &locked)
	if bal != 50 || locked != 100 {
		t.Errorf("balances after grant 150 + stake 100, no mint: balance=%v locked=%v, want 50/100", bal, locked)
	}
}

// (2) Tamper-evidence: a forged (wrong-key) or tampered receipt never verifies and never mints,
// even with minting ON.
func TestPoVINode_ForgedReceiptRejected_Integration(t *testing.T) {
	h := poviNodeHarness(t)
	ctx := context.Background()
	ws := "node-owner-ws"
	proc := povi.NewProcessor(povi.NewStore(h.pool), h.ledger, h.lookup, h.stakeMgr.IsEligible, true) // minting ON — still rejected

	// (a) signed with a DIFFERENT key than the one registered.
	nodeID, _, _ := h.registerNode(t, ws)
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	res, err := proc.Process(ctx, signedReceipt(wrongPriv, nodeID, ws))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified || res.Minted {
		t.Errorf("wrong-key receipt: Verified=%v Minted=%v, want false/false", res.Verified, res.Minted)
	}

	// (b) tampered AFTER signing — a single mutated field breaks the signature over CanonicalPayload.
	nodeID2, priv2, _ := h.registerNode(t, ws)
	r := signedReceipt(priv2, nodeID2, ws)
	r.OutputTokens++ // tamper
	res2, err := proc.Process(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Verified || res2.Minted {
		t.Errorf("tampered receipt: Verified=%v Minted=%v, want false/false", res2.Verified, res2.Minted)
	}
}

// (3) Would-mint isolation + U6-floor contrast (minting=true constructed IN-PROCESS only): a
// vouched+staked node mints > 0; an identical node WITHOUT the earn_verified vouch is gated by the
// U6 floor — the receipt still verifies + records, but mints nothing.
func TestPoVINode_WouldMint_FloorContrast_Integration(t *testing.T) {
	h := poviNodeHarness(t)
	ctx := context.Background()
	procOn := povi.NewProcessor(povi.NewStore(h.pool), h.ledger, h.lookup, h.stakeMgr.IsEligible, true) // in-process only
	procOn.SetMeasurementLookup(povi.NewMeasurementStore(h.pool))                                       // mint-basis gate (0099)

	// VOUCHED + staked + MEASURED → mints.
	wsV := "vouched-ws"
	nodeV, privV, _ := h.registerNode(t, wsV)
	h.vouch(t, wsV)
	h.bootstrapStake(t, ctx, wsV, nodeV)
	h.recordMeasurement(t, ctx, "req-"+nodeV, nodeV, wsV, 50) // Lens measured this served request
	resV, err := procOn.Process(ctx, signedReceipt(privV, nodeV, wsV))
	if err != nil {
		t.Fatalf("Process vouched: %v", err)
	}
	if !resV.Verified || !resV.StakeEligible || !resV.Minted || resV.Amount <= 0 {
		t.Fatalf("vouched + staked + measured node must MINT > 0 with minting ON; got %+v", resV)
	}

	// UNVOUCHED + staked + MEASURED → the U6 floor gates the mint; receipt verifies + records, mints nothing.
	wsU := "unvouched-ws"
	nodeU, privU, _ := h.registerNode(t, wsU)
	h.bootstrapStake(t, ctx, wsU, nodeU)                      // staked + eligible, but NEVER earn_verified
	h.recordMeasurement(t, ctx, "req-"+nodeU, nodeU, wsU, 50) // measured, so the mint reaches the U6 floor
	resU, err := procOn.Process(ctx, signedReceipt(privU, nodeU, wsU))
	if !errors.Is(err, mining.ErrEarnNotVerified) {
		t.Fatalf("unvouched mint must be gated by the U6 floor (ErrEarnNotVerified); got res=%+v err=%v", resU, err)
	}
	if !resU.Verified || !resU.StakeEligible {
		t.Errorf("unvouched receipt should still verify + be stake-eligible; got %+v", resU)
	}
	if resU.Minted {
		t.Errorf("unvouched node MUST NOT mint (U6 floor); got Minted=%v Amount=%v", resU.Minted, resU.Amount)
	}
}

// THE MONEY PROPERTY, end-to-end on real PG: a receipt claiming 10,000,000 output
// tokens against a request Lens MEASURED at 500 mints the amount for 500 — and
// the lens_token_ledger row's AMOUNT proves it (priced on measurement, not claim).
func TestPoVINode_MintPricedOnMeasurement_NotClaim_Integration(t *testing.T) {
	h := poviNodeHarness(t)
	ctx := context.Background()
	ws := "measured-ws"

	nodeID, priv, _ := h.registerNode(t, ws)
	h.vouch(t, ws)
	h.bootstrapStake(t, ctx, ws, nodeID)

	// Lens's OWN measurement of the served request: 500 output tokens.
	const measured = 500
	h.recordMeasurement(t, ctx, "req-inflated", nodeID, ws, measured)

	// The node signs a receipt CLAIMING 10,000,000 output tokens for that request.
	claim := povi.SignReceipt(priv, povi.Receipt{
		RequestID: "req-inflated", NodeID: nodeID, WorkspaceID: ws, Model: "test-model",
		InputTokens: 100, OutputTokens: 10_000_000, Timestamp: 1700000000, LeafCount: 50,
	})

	procOn := povi.NewProcessor(povi.NewStore(h.pool), h.ledger, h.lookup, h.stakeMgr.IsEligible, true)
	procOn.SetMeasurementLookup(povi.NewMeasurementStore(h.pool))
	res, err := procOn.Process(ctx, claim)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	wantMeasured := povi.ProvisionalMintAmountTokens(measured) // 25_000 µLENS
	wantClaim := povi.ProvisionalMintAmountTokens(10_000_000)  // 500_000_000 µLENS
	if !res.Minted || res.Amount != wantMeasured {
		t.Fatalf("Minted=%v Amount=%v, want minted with amount=%v (the MEASURED price)", res.Minted, res.Amount, wantMeasured)
	}

	// ON THE MONEY: the single receipt_mine_provisional ledger row is priced on
	// the MEASUREMENT (25_000), NOT the claim (500_000_000).
	var rows int
	var amount int64
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(SUM(amount),0) FROM lens_token_ledger
		 WHERE workspace_id=$1 AND type='receipt_mine_provisional'`, ws).Scan(&rows, &amount); err != nil {
		t.Fatalf("ledger read: %v", err)
	}
	if rows != 1 || amount != wantMeasured {
		t.Fatalf("ledger receipt_mine_provisional: rows=%d amount=%d, want 1 row of %d (measured)", rows, amount, wantMeasured)
	}
	if amount == wantClaim {
		t.Fatalf("ledger amount == %d == the CLAIM price — node OutputTokens reached the money", wantClaim)
	}
}

// FAIL CLOSED, end-to-end on real PG: a verified, staked, vouched receipt for a
// request Lens has NO measurement of mints NOTHING — asserted on the ABSENCE of
// any receipt_mine_provisional ledger row — while the receipt is still recorded.
func TestPoVINode_NoMeasurement_MintsNothing_Integration(t *testing.T) {
	h := poviNodeHarness(t)
	ctx := context.Background()
	ws := "unmeasured-ws"

	nodeID, priv, _ := h.registerNode(t, ws)
	h.vouch(t, ws)
	h.bootstrapStake(t, ctx, ws, nodeID)
	// Deliberately record NO measurement for this request.

	procOn := povi.NewProcessor(povi.NewStore(h.pool), h.ledger, h.lookup, h.stakeMgr.IsEligible, true)
	procOn.SetMeasurementLookup(povi.NewMeasurementStore(h.pool))
	res, err := procOn.Process(ctx, signedReceipt(priv, nodeID, ws))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Minted || res.Amount != 0 {
		t.Errorf("no measurement ⇒ mint nothing; got Minted=%v Amount=%v", res.Minted, res.Amount)
	}

	// ON THE MONEY: NO receipt_mine_provisional ledger row exists for this workspace.
	var minted bool
	if err := h.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM lens_token_ledger WHERE workspace_id=$1 AND type='receipt_mine_provisional')`, ws).
		Scan(&minted); err != nil {
		t.Fatalf("ledger read: %v", err)
	}
	if minted {
		t.Error("a receipt with NO gateway measurement must leave NO receipt_mine_provisional ledger row")
	}
	// The receipt is still RECORDED for audit even though it minted nothing.
	var recorded bool
	_ = h.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM povi_receipts WHERE request_id=$1)`, "req-"+nodeID).Scan(&recorded)
	if !recorded {
		t.Error("receipt must be recorded for audit even when unminted")
	}
}
