package poolroyalty_test

// eval_consensus_chain_integration_test.go — MINT-2 acceptance (real PG): a submitted eval earns NOTHING
// until INDEPENDENT operators attest its claimed answer is correct; once they do (and it is used to grade
// real model outputs) it mints HELD proportional to usage, is EXAMINED, and settles. The three farm defenses
// are proven end-to-end: an unconsensed item earns zero, an author's own sockpuppets cannot self-consense,
// and a self-certified WRONG answer (independent disagreement) never mints.
//
// It drives the REAL symbols: benchprobe.Store (the live submission surface: ContributeItem + ValidateItem +
// AttestCorrectness), poolroyalty.NewEvalContributionMinter (consensus-gated by default), and the
// examine→settle chain (detector + clearer + armed finalize sweeper).

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/benchprobe"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
)

func evalConsensusPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping eval-consensus chain test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS eval_correctness_attestations, eval_contribution_mints, benchmark_probes,
			inference_nodes, benchmark_eval_items, workspace_card_fingerprints, workspace_owner_links,
			lens_token_balances, lens_token_ledger`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT, metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE benchmark_eval_items (id TEXT PRIMARY KEY, input TEXT NOT NULL, expected_output TEXT NOT NULL,
			eval_method TEXT NOT NULL DEFAULT 'exact', pass_threshold DOUBLE PRECISION NOT NULL DEFAULT 1,
			active BOOLEAN NOT NULL DEFAULT false, content_hash TEXT, status TEXT NOT NULL DEFAULT 'pending',
			author_workspace_id TEXT, feature_category TEXT, input_token_range TEXT, complexity_bucket TEXT)`,
		`CREATE UNIQUE INDEX idx_eval_items_content_hash ON benchmark_eval_items (content_hash)`,
		`CREATE TABLE inference_nodes (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL)`,
		`CREATE TABLE benchmark_probes (id TEXT PRIMARY KEY, node_id TEXT NOT NULL, item_id TEXT NOT NULL,
			request_id TEXT, score DOUBLE PRECISION NOT NULL DEFAULT 0, UNIQUE (node_id, item_id))`,
		`CREATE TABLE workspace_card_fingerprints (workspace_id TEXT NOT NULL, fingerprint_hash TEXT NOT NULL)`,
		`CREATE TABLE workspace_owner_links (workspace_id TEXT NOT NULL, owner_key TEXT NOT NULL)`,
		`CREATE TABLE eval_correctness_attestations (item_id TEXT NOT NULL, attester_workspace_id TEXT NOT NULL,
			agrees BOOLEAN NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (item_id, attester_workspace_id))`,
		`CREATE TABLE eval_contribution_mints (request_id TEXT PRIMARY KEY, contributor_workspace_id TEXT NOT NULL,
			discrimination DOUBLE PRECISION NOT NULL, distinct_graders INTEGER NOT NULL, minted_amount BIGINT NOT NULL,
			status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

// submitEval uses the LIVE submission surface: ContributeItem (author-stamped, lands 'pending') then
// ValidateItem ('active', drawable + mint-eligible). Input is index-unique so content_hash never collides.
func submitEval(t *testing.T, store *benchprobe.Store, id, author, input, answer string) {
	t.Helper()
	ctx := context.Background()
	if err := store.ContributeItem(ctx, benchprobe.EvalItem{
		ID: id, Input: input, ExpectedOutput: answer, AuthorWorkspaceID: author,
	}); err != nil {
		t.Fatalf("ContributeItem(%s): %v", id, err)
	}
	if err := store.ValidateItem(ctx, id); err != nil {
		t.Fatalf("ValidateItem(%s): %v", id, err)
	}
}

// grade records that grader workspace `ws` used the item to score a model output (one node + one probe).
func grade(t *testing.T, pool *pgxpool.Pool, itemID, ws string, score float64) {
	t.Helper()
	ctx := context.Background()
	nodeID := fmt.Sprintf("node_%s_%s", itemID, ws)
	if _, err := pool.Exec(ctx, `INSERT INTO inference_nodes (id, workspace_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, nodeID, ws); err != nil {
		t.Fatalf("insert node: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO benchmark_probes (id, node_id, item_id, score) VALUES ($1,$2,$3,$4)`,
		nodeID+"_p", nodeID, itemID, score); err != nil {
		t.Fatalf("insert probe: %v", err)
	}
}

// linkOperator marks a↔b as the SAME operator (shared card fingerprint) — the identity edge the consensus
// gate closes over transitively.
func linkOperator(t *testing.T, pool *pgxpool.Pool, a, b, fp string) {
	t.Helper()
	for _, ws := range []string{a, b} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO workspace_card_fingerprints (workspace_id, fingerprint_hash) VALUES ($1,$2)`, ws, fp); err != nil {
			t.Fatalf("link fingerprint: %v", err)
		}
	}
}

func mintCount(t *testing.T, pool *pgxpool.Pool, author string) int {
	t.Helper()
	var n int
	_ = pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM eval_contribution_mints WHERE contributor_workspace_id=$1`, author).Scan(&n)
	return n
}

// TestEvalConsensusChain_Acceptance covers scenarios 1–3 + farm proofs 4b (same-operator) and 4c (wrong).
func TestEvalConsensusChain_Acceptance(t *testing.T) {
	pool := evalConsensusPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool) // nil verifier ⇒ U6 floor no-op (independently tested); consensus focus
	store := benchprobe.NewStore(pool)

	const rate = 10.0
	minter := poolroyalty.NewEvalContributionMinter(pool, ledger, rate, func() bool { return true })
	minter.SetHoldbackWindow(time.Millisecond) // finalize_after immediately past

	// GOOD item (author A): 3 distinct unlinked graders (1,1,0 → discriminating). Submitted + used, but NOT
	// yet attested.
	submitEval(t, store, "good", "wsA", "What is 2+2?", "4")
	grade(t, pool, "good", "wsG1", 1.0)
	grade(t, pool, "good", "wsG2", 1.0)
	grade(t, pool, "good", "wsG3", 0.0)

	// WRONG item (author A): self-certified wrong answer, also discriminating on usage, but independent
	// operators will DISAGREE with the claimed answer.
	submitEval(t, store, "wrong", "wsA", "What is 3+3?", "7") // claims 7 (wrong)
	grade(t, pool, "wrong", "wsG1", 1.0)
	grade(t, pool, "wrong", "wsG2", 1.0)
	grade(t, pool, "wrong", "wsG3", 0.0)

	// SOCKPUPPET item (author S): S funds S2, S3 (same operator, shared card). They grade AND attest.
	linkOperator(t, pool, "wsS", "wsS2", "card_S")
	linkOperator(t, pool, "wsS", "wsS3", "card_S")
	submitEval(t, store, "sock", "wsS", "What is 5+5?", "10")
	// Graders must be UNLINKED to clear discrimination warmup — use independent graders so discrimination is
	// NOT the blocker (isolating consensus as the reason it does not mint).
	grade(t, pool, "sock", "wsG1", 1.0)
	grade(t, pool, "sock", "wsG2", 1.0)
	grade(t, pool, "sock", "wsG3", 0.0)

	// ── SCENARIO 1: submitted + used, but UNCONSENSED → earns nothing. ──
	if n, err := minter.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (pre-consensus): %v", err)
	} else if n != 0 {
		t.Fatalf("pre-consensus: minted %d, want 0 (no item has correctness consensus yet)", n)
	}

	// ── Attestations. ──
	// GOOD: two INDEPENDENT operators agree the claimed answer is correct.
	mustAttest(t, store, "good", "wsB", true)
	mustAttest(t, store, "good", "wsC", true)
	// WRONG (proof 4c): two INDEPENDENT operators DISAGREE — the claimed answer is wrong.
	mustAttest(t, store, "wrong", "wsB", false)
	mustAttest(t, store, "wrong", "wsC", false)
	// SOCK (proof 4b): the author's OWN sockpuppets attest agree — same operator, must not count.
	mustAttest(t, store, "sock", "wsS2", true)
	mustAttest(t, store, "sock", "wsS3", true)

	// ── SCENARIO 2+3: only the GOOD item reaches consensus → mints HELD proportional to usage. ──
	n, err := minter.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce (post-consensus): %v", err)
	}
	if n != 1 {
		t.Fatalf("post-consensus: minted %d, want 1 (ONLY the consensus-reached, correct item)", n)
	}
	if got := mintCount(t, pool, "wsA"); got != 1 {
		t.Fatalf("author A mint rows=%d, want 1 (good minted, wrong withheld)", got)
	}
	// PROOF 4c: the self-certified WRONG item never minted.
	var wrongMinted int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM eval_contribution_mints WHERE request_id='wrong'`).Scan(&wrongMinted)
	if wrongMinted != 0 {
		t.Errorf("self-certified WRONG answer minted %d rows, want 0 (independent disagreement blocks it)", wrongMinted)
	}
	// PROOF 4b: the sockpuppet item never minted (author cannot self-consense through same-operator sisters).
	if got := mintCount(t, pool, "wsS"); got != 0 {
		t.Errorf("same-operator self-consensus minted %d rows, want 0 (transitive identity graph excludes them)", got)
	}

	// ── EXAMINE → SETTLE the GOOD held mint through the armed fail-closed chain. ──
	det := poolroyalty.NewSinglePartyConcentrationDetector(pool, "eval_contribution_mints", 100, 24*time.Hour)
	clearer := poolroyalty.NewSettlementClearer(det, pool, "eval_contribution_mints", func() bool { return true }, 24*time.Hour)
	if _, err := clearer.RunOnce(ctx); err != nil {
		t.Fatalf("clearer: %v", err)
	}
	time.Sleep(4 * time.Millisecond)
	sw := poolroyalty.NewFinalizeSweeper(pool, ledger, "eval_contribution_mints")
	sw.SetSettleStatus("cleared")
	if _, err := sw.RunOnce(ctx); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	// GOOD author settled ~ rate × 4·Var (scores 1,1,0 → 4·0.2222 ≈ 0.889 → ~8.89 LENS). Nothing strands.
	var bal, held int64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(balance,0), COALESCE(held_balance,0) FROM lens_token_balances WHERE workspace_id='wsA'`).
		Scan(&bal, &held)
	if bal < mining.FloatToMicroFloor(8.5) || bal > mining.FloatToMicroFloor(9.2) || held != 0 {
		t.Errorf("good author spendable=%d held=%d, want ~8.89 LENS / 0 (examined→cleared→settled)", bal, held)
	}
	var lingering int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM eval_contribution_mints WHERE status='held'`).Scan(&lingering)
	if lingering != 0 {
		t.Errorf("%d held mint rows linger, want 0 (nothing stranded)", lingering)
	}
}

func mustAttest(t *testing.T, store *benchprobe.Store, itemID, attester string, agrees bool) {
	t.Helper()
	if err := store.AttestCorrectness(context.Background(), itemID, attester, agrees); err != nil {
		t.Fatalf("AttestCorrectness(%s,%s): %v", itemID, attester, err)
	}
}

// TestEvalConsensusFarm_TenThousandUnusedEarnZero — proof 4a: 10,000 submitted-but-unused, unconsensed evals
// mint absolutely nothing. (Kept separate so the 500-row sweep window never crowds out the acceptance items.)
func TestEvalConsensusFarm_TenThousandUnusedEarnZero(t *testing.T) {
	pool := evalConsensusPool(t)
	ctx := context.Background()
	ledger := mining.NewLedgerStore(pool)

	// 10,000 active, authored items with NO graders and NO attestations — the pure farm attempt.
	if _, err := pool.Exec(ctx, `INSERT INTO benchmark_eval_items (id, input, expected_output, active, content_hash, status, author_workspace_id)
		SELECT 'farm_'||g, 'unused input '||g, 'x', true, 'hash_'||g, 'active', 'wsFarm'
		FROM generate_series(1,10000) g`); err != nil {
		t.Fatalf("seed 10k: %v", err)
	}

	minter := poolroyalty.NewEvalContributionMinter(pool, ledger, 10.0, func() bool { return true })
	minter.SetHoldbackWindow(time.Millisecond)
	// Several sweeps (each scans ≤500) — none may ever mint an unused/unconsensed item.
	for i := 0; i < 3; i++ {
		if n, err := minter.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		} else if n != 0 {
			t.Fatalf("sweep %d minted %d, want 0 (10,000 unused evals must earn nothing)", i, n)
		}
	}
	var mints int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM eval_contribution_mints`).Scan(&mints)
	if mints != 0 {
		t.Fatalf("eval_contribution_mints has %d rows, want 0 (no unused eval may ever mint)", mints)
	}
}
