package mining

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Real-PG proofs for annotation reputation (the aggregation/trigger SQL can't be exercised by
// the pgxmock harness). Gated on LENS_TEST_DATABASE_URL. Package mining → can reach the
// unexported reputation internals + parse the earning path for the money-boundary AST guard.

func repHarness(t *testing.T) (*pgxpool.Pool, *LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG reputation test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS reputation_events`,
		`DROP TABLE IF EXISTS annotations`,
		`DROP TABLE IF EXISTS annotation_tasks`,
		`DROP TABLE IF EXISTS annotator_stakes`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`CREATE TABLE annotation_tasks (id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text, source_workspace TEXT NOT NULL, prompt_hash TEXT NOT NULL DEFAULT '', response_a TEXT NOT NULL DEFAULT '', response_b TEXT NOT NULL DEFAULT '', task_type TEXT NOT NULL DEFAULT 'pairwise', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), expires_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE annotations (id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text, task_id TEXT NOT NULL REFERENCES annotation_tasks(id) ON DELETE CASCADE, annotator_id TEXT NOT NULL, decision TEXT NOT NULL, confidence INTEGER NOT NULL DEFAULT 3, time_spent_ms INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), UNIQUE (task_id, annotator_id))`,
		`CREATE TABLE annotator_stakes (workspace_id TEXT PRIMARY KEY, staked DOUBLE PRECISION NOT NULL DEFAULT 0, staked_at TIMESTAMPTZ)`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance DOUBLE PRECISION NOT NULL DEFAULT 0, held_balance DOUBLE PRECISION NOT NULL DEFAULT 0, lifetime_earned DOUBLE PRECISION NOT NULL DEFAULT 0, lifetime_spent DOUBLE PRECISION NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, amount DOUBLE PRECISION NOT NULL, balance_after DOUBLE PRECISION NOT NULL, type TEXT NOT NULL, description TEXT, metadata JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE reputation_events (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), annotator_id TEXT NOT NULL, kind TEXT NOT NULL, idem_key TEXT NOT NULL, delta DOUBLE PRECISION NOT NULL, reason JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), UNIQUE (annotator_id, kind, idem_key))`,
		// The append-only trigger (migration 0066), inline.
		`CREATE OR REPLACE FUNCTION reputation_events_block_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'reputation_events is append-only: % is blocked', TG_OP; END; $$`,
		`CREATE OR REPLACE TRIGGER reputation_events_no_mutation BEFORE UPDATE OR DELETE ON reputation_events FOR EACH ROW EXECUTE FUNCTION reputation_events_block_mutation()`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool, NewLedgerStoreForTesting(pool) // nil verifier → no U6 floor → real base+bonus earning
}

func seedTask(t *testing.T, pool *pgxpool.Pool, sourceWS string, expiresAt time.Time) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO annotation_tasks (source_workspace, expires_at) VALUES ($1,$2) RETURNING id`,
		sourceWS, expiresAt).Scan(&id); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return id
}

func seedAnnotation(t *testing.T, pool *pgxpool.Pool, taskID, annotatorID, decision string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO annotations (task_id, annotator_id, decision) VALUES ($1,$2,$3)`,
		taskID, annotatorID, decision); err != nil {
		t.Fatalf("seed annotation: %v", err)
	}
}

func scoreOf(t *testing.T, pool *pgxpool.Pool, ws string) float64 {
	t.Helper()
	s, err := reputationScore(context.Background(), pool, ws)
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	return s
}

// (1) COMPUTATION CORRECTNESS — baseline + summed deltas, clamped.
func TestReputation_ComputationCorrectness_Integration(t *testing.T) {
	pool, _ := repHarness(t)
	store := NewReputationStore(pool)
	if s := scoreOf(t, pool, "wsA"); s != ReputationBaseline {
		t.Errorf("no events: score %v want baseline %v", s, ReputationBaseline)
	}
	mustRecord(t, store, "wsA", "agreement_outcome", "t1", 0.10)
	mustRecord(t, store, "wsA", "agreement_outcome", "t2", 0.05)
	if s := scoreOf(t, pool, "wsA"); math.Abs(s-0.65) > 1e-9 {
		t.Errorf("score %v want 0.65 (0.5 + 0.10 + 0.05)", s)
	}
	mustRecord(t, store, "wsA", "decay", "d1", -1.0) // clamps to floor 0
	if s := scoreOf(t, pool, "wsA"); s != 0.0 {
		t.Errorf("clamp low: score %v want 0.0", s)
	}
}

func mustRecord(t *testing.T, s *ReputationStore, ws, kind, idem string, delta float64) {
	t.Helper()
	if err := s.recordEvent(context.Background(), ws, kind, idem, delta, map[string]any{}); err != nil {
		t.Fatalf("record: %v", err)
	}
}

// (2a) GAMING — easy-farm: many UNANIMOUS tasks → difficulty=0 → no gain.
func TestReputation_Gaming_EasyFarm_Integration(t *testing.T) {
	pool, _ := repHarness(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	for i := 0; i < 10; i++ {
		tid := seedTask(t, pool, "src", past)
		for _, w := range []string{"farmer", "w1", "w2", "w3"} {
			seedAnnotation(t, pool, tid, w, "a_better") // unanimous
		}
	}
	if _, err := NewReputationStore(pool).ResolveExpiredTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if s := scoreOf(t, pool, "farmer"); math.Abs(s-ReputationBaseline) > 1e-9 {
		t.Errorf("easy-farm: 10 unanimous tasks → score %v, want baseline (difficulty=0, zero gain)", s)
	}
}

// (2b) GAMING — consistent minority → score falls BELOW baseline.
func TestReputation_Gaming_Disagreement_Integration(t *testing.T) {
	pool, _ := repHarness(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		tid := seedTask(t, pool, "src", past)
		seedAnnotation(t, pool, tid, "bad", "b_better") // minority
		for _, w := range []string{"w1", "w2", "w3", "w4"} {
			seedAnnotation(t, pool, tid, w, "a_better") // majority 4/5
		}
	}
	if _, err := NewReputationStore(pool).ResolveExpiredTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if s := scoreOf(t, pool, "bad"); s >= ReputationBaseline {
		t.Errorf("consistent minority → score %v, want BELOW baseline %v", s, ReputationBaseline)
	}
}

// (2c) GAMING — collusion is BOUNDED (DOCUMENTED KNOWN-RESIDUAL).
// The difficulty + diversity weights bound a closed ring's gain; they do NOT eliminate it for
// a ring >= reputationDiversityFloor that can make a task look healthy. Reputation != money here,
// so the residual is not directly profitable — revisit IF reputation ever couples to money.
func TestReputation_Gaming_CollusionBounded_Integration(t *testing.T) {
	pool, _ := repHarness(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	// A 2-ring agreeing ONLY with each other (2-person unanimous) → difficulty=0 → ZERO gain.
	for i := 0; i < 10; i++ {
		tid := seedTask(t, pool, "src", past)
		seedAnnotation(t, pool, tid, "ring1", "a_better")
		seedAnnotation(t, pool, tid, "ring2", "a_better")
	}
	store := NewReputationStore(pool)
	if _, err := store.ResolveExpiredTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if s := scoreOf(t, pool, "ring1"); math.Abs(s-ReputationBaseline) > 1e-9 {
		t.Errorf("2-ring on unanimous tasks gained reputation (score %v) — difficulty weight should zero it", s)
	}
	// A 3-ring overcoming 1 honest dissenter gains only a SMALL BOUNDED amount per task.
	for i := 0; i < 5; i++ {
		tid := seedTask(t, pool, "src2", past)
		for _, w := range []string{"r1", "r2", "r3"} {
			seedAnnotation(t, pool, tid, w, "a_better")
		}
		seedAnnotation(t, pool, tid, "honest", "b_better")
	}
	if _, err := store.ResolveExpiredTasks(ctx); err != nil {
		t.Fatal(err)
	}
	s := scoreOf(t, pool, "r1")
	if s > 0.6 {
		t.Errorf("3-ring gain unbounded (score %v) — diversity/difficulty weights must bound it < 0.6", s)
	}
	if s <= ReputationBaseline {
		t.Errorf("3-ring should gain a SMALL positive amount (score %v) — the documented residual", s)
	}
}

// (3) RESOLUTION IDEMPOTENCY — a second resolve adds no events, score stable.
func TestReputation_ResolutionIdempotency_Integration(t *testing.T) {
	pool, _ := repHarness(t)
	ctx := context.Background()
	tid := seedTask(t, pool, "src", time.Now().Add(-time.Hour))
	seedAnnotation(t, pool, tid, "x", "a_better")
	seedAnnotation(t, pool, tid, "w1", "a_better")
	seedAnnotation(t, pool, tid, "w2", "b_better") // majority a_better 2/3
	store := NewReputationStore(pool)
	if n, _ := store.ResolveExpiredTasks(ctx); n != 1 {
		t.Fatalf("first resolve processed %d tasks, want 1", n)
	}
	var cnt1 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reputation_events`).Scan(&cnt1); err != nil {
		t.Fatal(err)
	}
	s1 := scoreOf(t, pool, "x")
	if n, _ := store.ResolveExpiredTasks(ctx); n != 0 {
		t.Errorf("second resolve processed %d tasks, want 0 (already resolved)", n)
	}
	var cnt2 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reputation_events`).Scan(&cnt2); err != nil {
		t.Fatal(err)
	}
	if cnt2 != cnt1 {
		t.Errorf("second resolve changed event count %d→%d (idempotency broken)", cnt1, cnt2)
	}
	if s2 := scoreOf(t, pool, "x"); math.Abs(s1-s2) > 1e-9 {
		t.Errorf("score changed on re-resolve %v→%v", s1, s2)
	}
}

// (4a) MONEY-BOUNDARY — AST guard: the earning path references no reputation symbol.
func TestReputation_MoneyBoundary_ASTGuard(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "annotation_mining.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	forbidden := map[string]bool{
		"reputationScore": true, "ReputationStore": true, "NewReputationStore": true,
		"ResolveExpiredTasks": true, "resolveTask": true, "ReputationBaseline": true, "recordEvent": true,
	}
	found := false
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "SubmitAnnotation" {
			continue
		}
		found = true
		ast.Inspect(fn, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && forbidden[id.Name] {
				t.Errorf("SubmitAnnotation (the earning path) references reputation symbol %q — earning must be reputation-free", id.Name)
			}
			return true
		})
	}
	if !found {
		t.Fatal("SubmitAnnotation not found in annotation_mining.go")
	}
}

// (4b) MONEY-BOUNDARY — INVARIANCE: opposite reputations, identical earning.
func TestReputation_MoneyBoundary_EarningInvariant_Integration(t *testing.T) {
	pool, ledger := repHarness(t)
	ctx := context.Background()
	miner := NewAnnotationMiner(ledger, pool)
	store := NewReputationStore(pool)
	// opposite reputations (via seeded events): wsHigh→0.9, wsLow→0.1.
	mustRecord(t, store, "wsHigh", "admin_reset", "r1", 0.4)
	mustRecord(t, store, "wsLow", "admin_reset", "r1", -0.4)
	if s := scoreOf(t, pool, "wsHigh"); math.Abs(s-0.9) > 1e-9 {
		t.Fatalf("setup: wsHigh score %v want 0.9", s)
	}
	if s := scoreOf(t, pool, "wsLow"); math.Abs(s-0.1) > 1e-9 {
		t.Fatalf("setup: wsLow score %v want 0.1", s)
	}
	// stake both above the requirement.
	for _, ws := range []string{"wsHigh", "wsLow"} {
		if _, err := pool.Exec(ctx, `INSERT INTO annotator_stakes (workspace_id, staked) VALUES ($1, 20)`, ws); err != nil {
			t.Fatal(err)
		}
	}
	// two IDENTICAL scenarios: a task with 3 pre-existing 'a_better'; the annotator submits 'a_better'
	// → agreement 1.0, otherCount 3 → base + high-agreement bonus.
	submit := func(ws string) {
		tid := seedTask(t, pool, "crowdsrc", time.Now().Add(time.Hour))
		for i := 0; i < 3; i++ {
			seedAnnotation(t, pool, tid, fmt.Sprintf("crowd-%s-%d", tid, i), "a_better")
		}
		if err := miner.SubmitAnnotation(ctx, Annotation{TaskID: tid, AnnotatorID: ws, Decision: "a_better", Confidence: 3}); err != nil {
			t.Fatalf("submit %s: %v", ws, err)
		}
	}
	submit("wsHigh")
	submit("wsLow")
	earn := func(ws string) float64 {
		var v float64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0) FROM lens_token_ledger WHERE workspace_id=$1 AND type=$2`, ws, TypeAnnotationMine).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	eh, el := earn("wsHigh"), earn("wsLow")
	if eh != el {
		t.Errorf("EARNING DEPENDS ON REPUTATION: high=%v low=%v — must be byte-identical", eh, el)
	}
	if math.Abs(eh-0.15) > 1e-9 {
		t.Errorf("earning %v, want 0.15 (base 0.1 + bonus 0.05, reputation-free)", eh)
	}
}

// (5) DISPLAY — GetAnnotatorStats returns the real computed score, not the old hardcoded 1.0.
func TestReputation_Display_RealScore_Integration(t *testing.T) {
	pool, ledger := repHarness(t)
	ctx := context.Background()
	miner := NewAnnotationMiner(ledger, pool)
	mustRecord(t, NewReputationStore(pool), "wsD", "admin_reset", "r1", 0.3) // → 0.8
	stats, err := miner.GetAnnotatorStats(ctx, "wsD")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(stats.Reputation-0.8) > 1e-9 {
		t.Errorf("display reputation %v, want 0.8 (the real computed score, not hardcoded 1.0)", stats.Reputation)
	}
}

// (6) APPEND-ONLY — the trigger rejects UPDATE and DELETE.
func TestReputation_AppendOnly_TriggerBlocks_Integration(t *testing.T) {
	pool, _ := repHarness(t)
	ctx := context.Background()
	mustRecord(t, NewReputationStore(pool), "wsT", "admin_reset", "r1", 0.1)
	if _, err := pool.Exec(ctx, `UPDATE reputation_events SET delta = 99 WHERE annotator_id='wsT'`); err == nil {
		t.Error("UPDATE on reputation_events must be blocked by the append-only trigger")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM reputation_events WHERE annotator_id='wsT'`); err == nil {
		t.Error("DELETE on reputation_events must be blocked by the append-only trigger")
	}
}
