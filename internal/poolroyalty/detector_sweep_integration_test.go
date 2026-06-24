package poolroyalty

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/talyvor/lens/internal/metrics"
)

// PR — the scheduled detector sweep. The mandatory proofs: findings precision,
// dedup/append-only, MONEY-SAFETY (the sweep never mutates a mint/balance), the
// import-guard (no ledger reachable), and the metrics gauge.

func swThresholds() DetectorThresholds {
	return DetectorThresholds{
		VolumeMinMints: 3, VolumeMaxRequesters: 1,
		BilateralMinFrac: 0.9, BilateralMinMints: 3,
		SimilarityMinSample: 3, SimilarityMaxStddev: 0.05,
	}
}

func swHarness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG detector sweep test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		// Drop dependent VIEWS first: pool_royalty_margin / distill_royalty_margin (created
		// by sibling margin tests, or left in a reused CI DB) depend on the mint tables, so
		// DROP TABLE below errors (2BP01) if they pre-exist. Ordering-independent (PR4 lesson).
		`DROP VIEW IF EXISTS pool_royalty_margin`,
		`DROP VIEW IF EXISTS distill_royalty_margin`,
		`DROP TABLE IF EXISTS royalty_detector_findings`,
		`DROP TABLE IF EXISTS pool_royalty_mints`,
		`DROP TABLE IF EXISTS distill_royalty_mints`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`CREATE TABLE pool_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE, requester_workspace_id TEXT NOT NULL, contributor_workspace_id TEXT NOT NULL, layer TEXT NOT NULL, entry_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', similarity DOUBLE PRECISION NOT NULL DEFAULT 0, avoided_cogs_usd DOUBLE PRECISION NOT NULL DEFAULT 0, minted_amount DOUBLE PRECISION NOT NULL DEFAULT 0, answer_sha256 TEXT NOT NULL DEFAULT '', prompt_sha256 TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'final', finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE distill_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), request_id TEXT NOT NULL UNIQUE, contributor_workspace_id TEXT NOT NULL, requester_workspace_id TEXT NOT NULL, content_hash TEXT NOT NULL, avoided_cogs_usd DOUBLE PRECISION NOT NULL, minted_amount DOUBLE PRECISION NOT NULL, status TEXT NOT NULL DEFAULT 'held', finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance DOUBLE PRECISION NOT NULL DEFAULT 0, held_balance DOUBLE PRECISION NOT NULL DEFAULT 0)`,
		`CREATE TABLE royalty_detector_findings (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), economy TEXT NOT NULL, detector TEXT NOT NULL, identity_key TEXT NOT NULL UNIQUE, contributor_workspace_id TEXT NOT NULL, requester_workspace_id TEXT, entry_or_content TEXT, window_seconds BIGINT NOT NULL, metrics JSONB NOT NULL, first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func swSeedCache(t *testing.T, pool *pgxpool.Pool, req, contrib, requester, entry, layer, prompt string, sim float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO pool_royalty_mints (request_id, contributor_workspace_id, requester_workspace_id, entry_id, layer, status, prompt_sha256, similarity, minted_amount)
		 VALUES ($1,$2,$3,$4,$5,'final',$6,$7,1.0)`,
		req, contrib, requester, entry, layer, prompt, sim); err != nil {
		t.Fatalf("seed cache %s: %v", req, err)
	}
}

func swSeedDistill(t *testing.T, pool *pgxpool.Pool, contrib, requester, content string) {
	t.Helper()
	rid := SHA256Hex([]byte(contrib + ":" + requester + ":" + content))
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO distill_royalty_mints (request_id, contributor_workspace_id, requester_workspace_id, content_hash, avoided_cogs_usd, minted_amount, status)
		 VALUES ($1,$2,$3,$4,2.0,1.0,'final') ON CONFLICT (request_id) DO NOTHING`,
		rid, contrib, requester, content); err != nil {
		t.Fatalf("seed distill %s/%s/%s: %v", contrib, requester, content, err)
	}
}

func newSweep(pool *pgxpool.Pool) *DetectorSweep {
	return NewDetectorSweep(
		NewDetectorReader(pool, swThresholds()),
		NewDistillDetectorReader(pool, swThresholds()),
		NewFindingsWriter(pool),
		24*time.Hour,
	)
}

func findingKeys(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT economy, detector, contributor_workspace_id FROM royalty_detector_findings`)
	if err != nil {
		t.Fatalf("query findings: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var e, d, c string
		if err := rows.Scan(&e, &d, &c); err != nil {
			t.Fatal(err)
		}
		out[e+"|"+d+"|"+c] = true
	}
	return out
}

// (1) FINDINGS PRECISION — gaming flagged, clean decoys NOT.
func TestDetectorSweep_FindingsPrecision_Integration(t *testing.T) {
	pool := swHarness(t)
	ctx := context.Background()

	// cache volume: E1, wsA→wsB ×3
	swSeedCache(t, pool, "cv1", "wsA", "wsB", "E1", "exact", "", 0)
	swSeedCache(t, pool, "cv2", "wsA", "wsB", "E1", "exact", "", 0)
	swSeedCache(t, pool, "cv3", "wsA", "wsB", "E1", "exact", "", 0)
	// cache bilateral: wsC→wsD ×3 distinct entries
	swSeedCache(t, pool, "cb1", "wsC", "wsD", "E3", "exact", "", 0)
	swSeedCache(t, pool, "cb2", "wsC", "wsD", "E4", "exact", "", 0)
	swSeedCache(t, pool, "cb3", "wsC", "wsD", "E5", "exact", "", 0)
	// cache similarity: semantic cluster wsS/ES ×3, distinct prompts, tight sim
	swSeedCache(t, pool, "cs1", "wsS", "wsT", "ES", "semantic", "p1", 0.95)
	swSeedCache(t, pool, "cs2", "wsS", "wsU", "ES", "semantic", "p2", 0.95)
	swSeedCache(t, pool, "cs3", "wsS", "wsV", "ES", "semantic", "p3", 0.96)
	// cache CLEAN: E2, wsClean→3 distinct requesters
	swSeedCache(t, pool, "cc1", "wsClean", "q1", "E2", "exact", "", 0)
	swSeedCache(t, pool, "cc2", "wsClean", "q2", "E2", "exact", "", 0)
	swSeedCache(t, pool, "cc3", "wsClean", "q3", "E2", "exact", "", 0)
	// distill swarm: content hot, wsDA, 4 requesters
	swSeedDistill(t, pool, "wsDA", "dr1", "hot")
	swSeedDistill(t, pool, "wsDA", "dr2", "hot")
	swSeedDistill(t, pool, "wsDA", "dr3", "hot")
	swSeedDistill(t, pool, "wsDA", "dr4", "hot")
	// distill bilateral: wsDC→wsDD ×3 distinct contents
	swSeedDistill(t, pool, "wsDC", "wsDD", "k1")
	swSeedDistill(t, pool, "wsDC", "wsDD", "k2")
	swSeedDistill(t, pool, "wsDC", "wsDD", "k3")
	// distill CLEAN: content cool, wsDClean→2 requesters
	swSeedDistill(t, pool, "wsDClean", "z1", "cool")
	swSeedDistill(t, pool, "wsDClean", "z2", "cool")

	if err := newSweep(pool).RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got := findingKeys(t, pool)
	for _, want := range []string{
		"cache|volume|wsA", "cache|bilateral|wsC", "cache|similarity|wsS",
		"distill|volume|wsDA", "distill|bilateral|wsDC",
	} {
		if !got[want] {
			t.Errorf("FALSE EXCLUSION — expected finding missing: %s", want)
		}
	}
	for k := range got {
		if strings.Contains(k, "wsClean") || strings.Contains(k, "wsDClean") {
			t.Errorf("FALSE INCLUSION — clean contributor flagged: %s", k)
		}
	}

	// evidence present in the JSONB
	var metricsJSON string
	if err := pool.QueryRow(ctx, `SELECT metrics::text FROM royalty_detector_findings WHERE economy='cache' AND detector='volume' LIMIT 1`).Scan(&metricsJSON); err != nil {
		t.Fatalf("metrics jsonb: %v", err)
	}
	if !strings.Contains(metricsJSON, "EntryTotalMints") {
		t.Errorf("metrics JSONB missing evidence; got %s", metricsJSON)
	}

	// metrics gauge equals the flagged count (cache volume: 1 flagged tuple).
	if v := testutil.ToFloat64(metrics.RoyaltyDetectorFlagged.WithLabelValues("cache", "volume")); v != 1 {
		t.Errorf("gauge cache/volume=%v want 1", v)
	}
	if v := testutil.ToFloat64(metrics.RoyaltyDetectorFlagged.WithLabelValues("distill", "volume")); v != 4 {
		t.Errorf("gauge distill/volume=%v want 4 (swarm: 4 flagged tuples)", v)
	}
}

// clean-only → zero findings.
func TestDetectorSweep_CleanNoFindings_Integration(t *testing.T) {
	pool := swHarness(t)
	swSeedCache(t, pool, "x1", "w1", "q1", "E2", "exact", "", 0)
	swSeedCache(t, pool, "x2", "w1", "q2", "E2", "exact", "", 0)
	swSeedDistill(t, pool, "w2", "z1", "cool")
	if err := newSweep(pool).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM royalty_detector_findings`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("clean traffic produced %d findings, want 0", n)
	}
}

// (2) DEDUP / APPEND-ONLY — a second sweep adds no rows and never updates first_seen_at.
func TestDetectorSweep_DedupAppendOnly_Integration(t *testing.T) {
	pool := swHarness(t)
	ctx := context.Background()
	swSeedCache(t, pool, "d1", "wsA", "wsB", "E1", "exact", "", 0)
	swSeedCache(t, pool, "d2", "wsA", "wsB", "E1", "exact", "", 0)
	swSeedCache(t, pool, "d3", "wsA", "wsB", "E1", "exact", "", 0)

	s := newSweep(pool)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var n1 int
	var firstSeen1 time.Time
	mustScan(t, pool, `SELECT count(*) FROM royalty_detector_findings`, &n1)
	mustScan(t, pool, `SELECT first_seen_at FROM royalty_detector_findings WHERE economy='cache' AND detector='volume' LIMIT 1`, &firstSeen1)
	if n1 == 0 {
		t.Fatal("expected findings after first sweep")
	}

	if err := s.RunOnce(ctx); err != nil { // second sweep
		t.Fatal(err)
	}
	var n2 int
	var firstSeen2 time.Time
	mustScan(t, pool, `SELECT count(*) FROM royalty_detector_findings`, &n2)
	mustScan(t, pool, `SELECT first_seen_at FROM royalty_detector_findings WHERE economy='cache' AND detector='volume' LIMIT 1`, &firstSeen2)
	if n2 != n1 {
		t.Errorf("second sweep changed row count %d→%d (dedup broken)", n1, n2)
	}
	if !firstSeen1.Equal(firstSeen2) {
		t.Errorf("first_seen_at mutated %v→%v (append-only broken — cron must never UPDATE)", firstSeen1, firstSeen2)
	}
}

// (3) MONEY-SAFETY (mandatory) — the money tables are byte-identical after a sweep.
func moneySnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT request_id||'|'||status FROM pool_royalty_mints ORDER BY request_id`,
		`SELECT request_id||'|'||status FROM distill_royalty_mints ORDER BY request_id`,
		`SELECT workspace_id||'|'||balance::text||'|'||held_balance::text FROM lens_token_balances ORDER BY workspace_id`,
	} {
		rows, err := pool.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			b.WriteString(s)
			b.WriteByte('\n')
		}
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

func TestDetectorSweep_MoneySafety_Integration(t *testing.T) {
	pool := swHarness(t)
	ctx := context.Background()
	// gaming rows in BOTH economies + a balance row.
	swSeedCache(t, pool, "m1", "wsA", "wsB", "E1", "exact", "", 0)
	swSeedCache(t, pool, "m2", "wsA", "wsB", "E1", "exact", "", 0)
	swSeedCache(t, pool, "m3", "wsA", "wsB", "E1", "exact", "", 0)
	swSeedDistill(t, pool, "wsDA", "dr1", "hot")
	swSeedDistill(t, pool, "wsDA", "dr2", "hot")
	swSeedDistill(t, pool, "wsDA", "dr3", "hot")
	swSeedDistill(t, pool, "wsDA", "dr4", "hot")
	if _, err := pool.Exec(ctx, `INSERT INTO lens_token_balances (workspace_id, balance, held_balance) VALUES ('wsA', 12.5, 4.0)`); err != nil {
		t.Fatal(err)
	}

	before := moneySnapshot(t, pool)
	if err := newSweep(pool).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	after := moneySnapshot(t, pool)
	if before != after {
		t.Errorf("MONEY TABLES MUTATED by the sweep — never-auto-act violated.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// (4) IMPORT-GUARD — detector_sweep.go imports no ledger, and neither sweep file
// references a mutation primitive (the never-auto-act structural proof).
func TestDetectorSweep_NeverActs_ImportGuard(t *testing.T) {
	// (a) imports of detector_sweep.go must not include the ledger package.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "detector_sweep.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(p, "internal/mining") {
			t.Errorf("detector_sweep.go imports the ledger %q — must not (no mint/burn reachable)", p)
		}
	}
	// (b) neither sweep file references a mutation primitive IN CODE (AST identifiers,
	// so the doc-comments that NAME these primitives to describe the guarantee don't
	// trip the guard — only an actual code reference would). Catches same-package use.
	forbidden := map[string]bool{
		"RevokeHeldTx": true, "CreditHeldTx": true, "FinalizeHeldTx": true,
		"RevokeHeldMints": true, "NewRevoker": true, "NewAdjudicationWriter": true, "Adjudicate": true,
	}
	for _, file := range []string{"detector_sweep.go", "findings_writer.go"} {
		af, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbidden[id.Name] {
				t.Errorf("%s references mutation primitive %q in CODE — the sweep must be read-only/append-findings-only", file, id.Name)
			}
			return true
		})
	}
}
