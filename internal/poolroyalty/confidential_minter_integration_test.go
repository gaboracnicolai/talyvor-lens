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

// Real-PG proofs for proof-of-confidential-compute (Proof-of-Improvement instance 4). Pays a FLAT
// rate-per-epoch for VERIFIED confidential capacity — gated by (attestation verified + key_bound=true + not
// expired) AND held-probe correctness. NEVER reads gpu_type or any latency signal (no double-pay). The clock
// is injected (m.now) for deterministic epochs. Note: the harness deliberately does NOT create
// node_cohort_latency_stats — if the minter read latency, its scan would error, so a green run PROVES no
// latency dependency (proof 3).

var confBaseTime = time.Unix(1_000_000_000, 0)

func confHarness(t *testing.T) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG confidential mint test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_confidmint_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_confidmint_test CASCADE`,
		`CREATE SCHEMA lens_confidmint_test`,
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
		`CREATE TABLE benchmark_node_scores (node_id TEXT NOT NULL, model TEXT NOT NULL, score DOUBLE PRECISION NOT NULL,
			sample_count INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (node_id, model))`,
		`CREATE TABLE node_attestations (nonce BIGINT PRIMARY KEY, node_id TEXT NOT NULL, attestation_status TEXT NOT NULL,
			attested_gpu_class TEXT, cc_mode BOOLEAN, eat_digest TEXT, key_bound BOOLEAN NOT NULL DEFAULT false,
			attested_at TIMESTAMPTZ NOT NULL DEFAULT now(), expires_at TIMESTAMPTZ)`,
		`CREATE TABLE confidential_compute_mints (request_id TEXT PRIMARY KEY, contributor_workspace_id TEXT NOT NULL,
			minted_amount DOUBLE PRECISION NOT NULL, node_id TEXT NOT NULL, attested_gpu_class TEXT NOT NULL, epoch BIGINT NOT NULL,
			status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := mining.NewLedgerStore(pool)
	ledger.SetMintVerifier(earnverify.New())
	return pool, ledger
}

var attNonce int64 = 1

// seedAttestation inserts a node_attestations row; keyBound + status + expiry are the eligibility knobs.
func seedAttestation(t *testing.T, pool *pgxpool.Pool, nodeID, class, status string, keyBound bool, expiresAt time.Time) {
	t.Helper()
	attNonce++
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO node_attestations (nonce, node_id, attestation_status, attested_gpu_class, cc_mode, key_bound, expires_at)
		 VALUES ($1,$2,$3,$4,true,$5,$6)`, attNonce, nodeID, status, class, keyBound, expiresAt); err != nil {
		t.Fatal(err)
	}
}

func confMintRows(t *testing.T, pool *pgxpool.Pool, ws string) (n int, amount float64) {
	t.Helper()
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(sum(minted_amount),0) FROM confidential_compute_mints WHERE contributor_workspace_id=$1`, ws).Scan(&n, &amount)
	return
}

func newConfMinterAt(pool *pgxpool.Pool, ledger ledgerCreditTx, rate float64, at time.Time) *ConfidentialMinter {
	m := NewConfidentialMinter(pool, ledger, rate, func() bool { return true })
	m.now = func() time.Time { return at }
	return m
}

// eligible node: verified + key_bound=true + not expired attestation, passing benchmark, verified workspace.
func seedEligible(t *testing.T, pool *pgxpool.Pool, node, ws, class string) {
	t.Helper()
	seedNode(t, pool, node, ws) // inference_nodes + verifyWorkspace (shared helper)
	seedBenchmark(t, pool, node, "gpt-4o", 0.9, 10)
	seedAttestation(t, pool, node, class, "verified", true, time.Now().Add(48*time.Hour))
}

// (proof 1) MINT-CORRECTNESS: an eligible node mints EXACTLY rate × 1.0 (flat verified-capacity reward). rate=10 ⇒ 10.0.
func TestConfidential_Correctness_ExactAmount(t *testing.T) {
	pool, ledger := confHarness(t)
	ctx := context.Background()
	seedEligible(t, pool, "nodeA", "wsA", "H100")

	m := newConfMinterAt(pool, ledger, 10, confBaseTime)
	n, err := m.RunOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("eligible node must mint exactly 1: n=%d err=%v", n, err)
	}
	rows, amount := confMintRows(t, pool, "wsA")
	if rows != 1 || amount < 9.999 || amount > 10.001 {
		t.Fatalf("expected 1 mint of 10.0 (rate 10 × capacity 1.0): rows=%d amount=%.4f", rows, amount)
	}
	if _, held := balances(t, pool, "wsA"); held < 9.999 || held > 10.001 {
		t.Fatalf("wsA held_balance %.4f, want 10.0", held)
	}
}

// (proof 2) THE ATTESTATION GATE — the whole point.
func TestConfidential_AttestationGate(t *testing.T) {
	pool, ledger := confHarness(t)
	ctx := context.Background()
	m := newConfMinterAt(pool, ledger, 10, confBaseTime)

	// (a) no attestation row ⇒ zero.
	seedNode(t, pool, "noAtt", "wsNo")
	seedBenchmark(t, pool, "noAtt", "gpt-4o", 0.9, 10)
	// (b) verified but key_bound=FALSE (relay-fenced) ⇒ zero.
	seedNode(t, pool, "unbound", "wsUnbound")
	seedBenchmark(t, pool, "unbound", "gpt-4o", 0.9, 10)
	seedAttestation(t, pool, "unbound", "H100", "verified", false, time.Now().Add(48*time.Hour))
	// (c) verified+key_bound but EXPIRED ⇒ zero.
	seedNode(t, pool, "expired", "wsExpired")
	seedBenchmark(t, pool, "expired", "gpt-4o", 0.9, 10)
	seedAttestation(t, pool, "expired", "H100", "verified", true, time.Now().Add(-time.Hour))
	// (d) fully eligible ⇒ pays.
	seedEligible(t, pool, "good", "wsGood", "H100")

	if _, err := m.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{"wsNo", "wsUnbound", "wsExpired"} {
		if n, _ := confMintRows(t, pool, ws); n != 0 {
			t.Fatalf("%s must mint 0 (attestation gate), got %d", ws, n)
		}
	}
	if n, _ := confMintRows(t, pool, "wsGood"); n != 1 {
		t.Fatalf("fully-eligible node must mint 1, got %d", n)
	}
}

// (proof 4) C-GATE: unscored or below-threshold node ⇒ zero.
func TestConfidential_CGate(t *testing.T) {
	pool, ledger := confHarness(t)
	ctx := context.Background()
	// attested + key_bound but NO benchmark score ⇒ zero.
	seedNode(t, pool, "unscored", "wsUnscored")
	seedAttestation(t, pool, "unscored", "H100", "verified", true, time.Now().Add(48*time.Hour))
	// attested + key_bound but benchmark BELOW threshold ⇒ zero.
	seedNode(t, pool, "lowscore", "wsLow")
	seedBenchmark(t, pool, "lowscore", "gpt-4o", 0.3, 10)
	seedAttestation(t, pool, "lowscore", "H100", "verified", true, time.Now().Add(48*time.Hour))

	m := newConfMinterAt(pool, ledger, 10, confBaseTime)
	if _, err := m.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{"wsUnscored", "wsLow"} {
		if n, _ := confMintRows(t, pool, ws); n != 0 {
			t.Fatalf("%s must mint 0 (C-gate), got %d", ws, n)
		}
	}
}

// (proof 5) EXACTLY-ONCE per epoch + next epoch re-pays.
func TestConfidential_ExactlyOnce_NextEpoch(t *testing.T) {
	pool, ledger := confHarness(t)
	ctx := context.Background()
	seedEligible(t, pool, "nodeA", "wsA", "H100")
	m := newConfMinterAt(pool, ledger, 10, confBaseTime)

	if n, _ := m.RunOnce(ctx); n != 1 {
		t.Fatalf("epoch E: want 1, got %d", n)
	}
	if n, _ := m.RunOnce(ctx); n != 0 {
		t.Fatalf("epoch E again: want 0 (ON CONFLICT), got %d", n)
	}
	m.now = func() time.Time { return confBaseTime.Add(time.Duration(m.windowSeconds) * time.Second) }
	if n, _ := m.RunOnce(ctx); n != 1 {
		t.Fatalf("next epoch: want a fresh mint, got %d", n)
	}
	if rows, _ := confMintRows(t, pool, "wsA"); rows != 2 {
		t.Fatalf("wsA want 2 mints across 2 epochs, got %d", rows)
	}
}

// (proof 6 + 8) INERT by flag/rate AND by SUBSTRATE ABSENCE.
func TestConfidential_Inert_And_SubstrateAbsence(t *testing.T) {
	pool, ledger := confHarness(t)
	ctx := context.Background()
	seedEligible(t, pool, "nodeA", "wsA", "H100")

	// rate-0 ⇒ nil anchor ⇒ zero.
	inert := newConfMinterAt(pool, ledger, 0, confBaseTime)
	if n, err := inert.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("rate-0 must mint 0: n=%d err=%v", n, err)
	}
	if n, _ := confMintRows(t, pool, "wsA"); n != 0 {
		t.Fatalf("rate-0 must write zero rows, got %d", n)
	}
	// flag-off ⇒ no-op.
	off := NewConfidentialMinter(pool, ledger, 10, func() bool { return false })
	off.now = func() time.Time { return confBaseTime }
	if n, err := off.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("flag-off must no-op: n=%d err=%v", n, err)
	}

	// (proof 8) SUBSTRATE ABSENCE: remove all key_bound=true rows (today's reality — no CC hardware). With
	// the flag ON + a positive rate, RunOnce STILL mints ZERO — inert-by-substrate-absence, why this is merge-held.
	if _, err := pool.Exec(ctx, `UPDATE node_attestations SET key_bound=false`); err != nil {
		t.Fatal(err)
	}
	live := newConfMinterAt(pool, ledger, 10, confBaseTime)
	if n, err := live.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("no key_bound=true rows ⇒ ZERO mints even flag+rate on: n=%d err=%v", n, err)
	}
}

// (proof 2/U6) U6 floor: an unverified node-workspace with a fully-valid attestation ⇒ zero (rollback); the
// credit type is TypeConfidentialComputeHeld ∈ mintTypeList.
func TestConfidential_U6Floor(t *testing.T) {
	pool, ledger := confHarness(t)
	ctx := context.Background()
	// node registered but its workspace is NOT verified.
	if _, err := pool.Exec(ctx, `INSERT INTO inference_nodes (id, workspace_id) VALUES ('nodeU','wsUnv')`); err != nil {
		t.Fatal(err)
	}
	seedBenchmark(t, pool, "nodeU", "gpt-4o", 0.9, 10)
	seedAttestation(t, pool, "nodeU", "H100", "verified", true, time.Now().Add(48*time.Hour))
	m := newConfMinterAt(pool, ledger, 10, confBaseTime)
	if n, err := m.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("unverified workspace ⇒ 0 (U6 rollback): n=%d err=%v", n, err)
	}
	if n, _ := confMintRows(t, pool, "wsUnv"); n != 0 {
		t.Fatalf("U6: claim must roll back, got %d", n)
	}
	verifyWorkspace(t, pool, "wsUnv")
	if n, _ := m.RunOnce(ctx); n != 1 {
		t.Fatal("verified workspace must now mint")
	}
	var typ string
	_ = pool.QueryRow(ctx, `SELECT type FROM lens_token_ledger WHERE workspace_id='wsUnv' ORDER BY created_at DESC LIMIT 1`).Scan(&typ)
	if typ != mining.TypeConfidentialComputeHeld {
		t.Fatalf("ledger type = %q, want %q", typ, mining.TypeConfidentialComputeHeld)
	}
}
