package eval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talyvor/lens/internal/quality"
)

// planted_case_execution_test.go — THE CONSEQUENCE HALF of W6.14, executed.
//
// cmd/lens/eval_case_create_handler_test.go proves who may create a test case in
// which workspace. This proves what a stored case DOES when that workspace runs
// its suite, because "a row landed in the wrong workspace" and "somebody else
// chooses which model you pay for" are different findings and only the second one
// is a money path.
//
// RunSuite(ctx, workspaceID, tags) lists every case in the workspace and runs each
// one; runTestCaseWith calls callLLM(ctx, tc.Provider, tc.Model, tc.Prompt) with
// the provider, model and prompt taken from the STORED CASE. This drives that with
// the real Pipeline against an httptest provider and records exactly what went out.
//
// ⚠ Nothing here asserts a policy or changes behaviour — it records the mechanism
// so the create-side fix has a measured reason.

type providerRecorder struct {
	mu    sync.Mutex
	calls []map[string]any
	srv   *httptest.Server
}

func newProviderRecorder(t *testing.T) *providerRecorder {
	t.Helper()
	p := &providerRecorder{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		p.calls = append(p.calls, body)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *providerRecorder) seen() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.calls...)
}

// plantedCaseHarness mirrors migration 0012 for the three tables RunSuite touches.
// ⚠ A REAL POOL IS REQUIRED AND THAT IS THE POINT: ListTestCases returns nil when
// p.pool is nil, so a pool-free Pipeline runs ZERO cases and every assertion below
// would pass while measuring nothing. The first draft of this file did exactly
// that and the non-vacuity check caught it.
func plantedCaseHarness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG planted-case execution test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS eval_results`,
		`DROP TABLE IF EXISTS eval_runs`,
		`DROP TABLE IF EXISTS eval_test_cases`,
		`CREATE TABLE eval_test_cases (
			id              TEXT PRIMARY KEY,
			name            TEXT NOT NULL,
			workspace_id    TEXT NOT NULL DEFAULT 'default',
			provider        TEXT NOT NULL,
			model           TEXT NOT NULL,
			prompt          TEXT NOT NULL,
			expected_output TEXT NOT NULL DEFAULT '',
			eval_method     TEXT NOT NULL DEFAULT 'heuristic',
			pass_threshold  FLOAT NOT NULL DEFAULT 0.6,
			tags            TEXT[] NOT NULL DEFAULT '{}',
			created_at      TIMESTAMPTZ DEFAULT NOW())`,
		`CREATE TABLE eval_results (
			id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			test_case_id TEXT NOT NULL,
			run_id       TEXT NOT NULL,
			passed       BOOLEAN NOT NULL,
			score        FLOAT NOT NULL,
			latency_ms   INTEGER NOT NULL DEFAULT 0,
			cost_usd     FLOAT NOT NULL DEFAULT 0,
			eval_method  TEXT NOT NULL,
			error        TEXT NOT NULL DEFAULT '',
			created_at   TIMESTAMPTZ DEFAULT NOW())`,
		`CREATE TABLE eval_runs (
			id             TEXT PRIMARY KEY,
			workspace_id   TEXT NOT NULL DEFAULT 'default',
			total_tests    INTEGER NOT NULL DEFAULT 0,
			passed         INTEGER NOT NULL DEFAULT 0,
			failed         INTEGER NOT NULL DEFAULT 0,
			pass_rate      FLOAT NOT NULL DEFAULT 0,
			total_cost_usd FLOAT NOT NULL DEFAULT 0,
			avg_latency_ms INTEGER NOT NULL DEFAULT 0,
			created_at     TIMESTAMPTZ DEFAULT NOW(),
			completed_at   TIMESTAMPTZ)`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func TestRunSuite_ExecutesEveryStoredCaseWithItsOwnProviderModelAndPrompt(t *testing.T) {
	rec := newProviderRecorder(t)
	pool := plantedCaseHarness(t)
	p := newPipeline(pool, quality.New(nil), "operator-openai-key", "", "")
	p.openAIURL = rec.srv.URL

	ctx := context.Background()
	if _, err := p.AddTestCase(ctx, TestCase{
		Name: "planted", WorkspaceID: "ws-victim",
		Provider: "openai", Model: "gpt-4o",
		Prompt: "ATTACKER-CHOSEN-PROMPT", ExpectedOutput: "ok",
		EvalMethod: EvalExact, PassThreshold: 1,
	}); err != nil {
		t.Fatalf("AddTestCase: %v", err)
	}

	summary, err := p.RunSuite(ctx, "ws-victim", nil)
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}

	// NON-VACUITY: the suite really ran a case. Otherwise "no attacker prompt went
	// out" would be true because nothing went out at all.
	if summary.TotalTests != 1 {
		t.Fatalf("RunSuite ran %d cases, want 1 — nothing was executed, so this test measures "+
			"nothing", summary.TotalTests)
	}
	calls := rec.seen()
	if len(calls) == 0 {
		t.Fatal("the provider was never called, so 'a stored case reaches a provider' is untested")
	}

	// THE MECHANISM: model and prompt on the wire are the ones the CASE carried.
	raw, _ := json.Marshal(calls[0])
	if !strings.Contains(string(raw), "gpt-4o") {
		t.Errorf("the model on the wire is not the one the stored case named: %s", raw)
	}
	if !strings.Contains(string(raw), "ATTACKER-CHOSEN-PROMPT") {
		t.Errorf("the prompt on the wire is not the one the stored case carried: %s", raw)
	}
	// And it is costed. Stated precisely rather than generously: the charge is
	// real (a provider call went out on the operator's key) and the RECORDED
	// figure comes from alerts.CostUSD, which returns 0 for any model the catalog
	// does not price. ⚠ MEASURED, AND THE LIST IS NOT WHAT YOU WOULD GUESS:
	// CostUSD("gpt-4o",...) is non-zero, while "gpt-4", "gpt-4-turbo" and
	// "claude-3-opus" all return 0 — real, expensive model names that record
	// nothing. This test therefore uses a PRICED model, so "the run is costed" is
	// a claim it actually demonstrates. (The unpriced-model behaviour is a
	// separate finding and is handed on, not fixed here: eval calls the plain
	// alerts.CostUSD, which has no WarnUnpricedModel path, unlike
	// alerts.CostUSDResolved.)
	if summary.TotalCostUSD <= 0 {
		t.Errorf("summary.TotalCostUSD = %v, want > 0 for a known model — if this is 0 the "+
			"'planted cases cost money' claim must be restated as 'they make calls whose "+
			"recorded cost is 0'", summary.TotalCostUSD)
	}
	t.Logf("RunSuite(ws-victim) called %q with the stored case's model and prompt, on the "+
		"operator's provider key; summary.TotalCostUSD=%v", rec.srv.URL, summary.TotalCostUSD)
}

// The scoping half of the same mechanism, so the finding is stated precisely: a
// case belonging to ANOTHER workspace is NOT run. The whole exposure therefore
// comes from being able to write into the victim's workspace in the first place —
// which is what cmd/lens/eval_case_create_handler.go now prevents.
func TestRunSuite_DoesNotRunAnotherWorkspacesCases(t *testing.T) {
	rec := newProviderRecorder(t)
	pool := plantedCaseHarness(t)
	p := newPipeline(pool, quality.New(nil), "operator-openai-key", "", "")
	p.openAIURL = rec.srv.URL

	ctx := context.Background()
	if _, err := p.AddTestCase(ctx, TestCase{
		Name: "attackers-own", WorkspaceID: "ws-attacker",
		Provider: "openai", Model: "m", Prompt: "ELSEWHERE",
		EvalMethod: EvalExact, PassThreshold: 1,
	}); err != nil {
		t.Fatalf("AddTestCase: %v", err)
	}
	if _, err := p.AddTestCase(ctx, TestCase{
		Name: "victims-own", WorkspaceID: "ws-victim",
		Provider: "openai", Model: "m", Prompt: "MINE",
		EvalMethod: EvalExact, PassThreshold: 1,
	}); err != nil {
		t.Fatalf("AddTestCase: %v", err)
	}

	summary, err := p.RunSuite(ctx, "ws-victim", nil)
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if summary.TotalTests != 1 {
		t.Fatalf("RunSuite(ws-victim) ran %d cases, want exactly 1 — the workspace filter on "+
			"ListTestCases is not binding", summary.TotalTests)
	}
	for _, c := range rec.seen() {
		raw, _ := json.Marshal(c)
		if strings.Contains(string(raw), "ELSEWHERE") {
			t.Errorf("ws-victim's run executed a case stored under ws-attacker: %s", raw)
		}
	}
}
