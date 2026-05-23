package eval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/quality"
)

func newPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// openAIMock returns an httptest server that responds with the given
// assistant content. If responder is non-nil it can override the body
// per request — useful for the LLM-judge test where the same server
// answers both the test-case call and the judge call.
func openAIMock(t *testing.T, responder func(model string) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		content := responder(req.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+strconvQuote(content)+`}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestAddTestCase_ValidatesEmptyName(t *testing.T) {
	pool := newPool(t)
	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	_, err := p.AddTestCase(context.Background(), TestCase{Name: "", Prompt: "hi", Provider: "openai", Model: "gpt-4"})
	if err == nil {
		t.Error("AddTestCase with empty Name must error")
	}
	_, err = p.AddTestCase(context.Background(), TestCase{Name: "ok", Prompt: "", Provider: "openai", Model: "gpt-4"})
	if err == nil {
		t.Error("AddTestCase with empty Prompt must error")
	}
}

func TestAddTestCase_InsertsWithCorrectFields(t *testing.T) {
	pool := newPool(t)
	pool.ExpectExec(`INSERT INTO eval_test_cases`).
		WithArgs(
			pgxmock.AnyArg(), // id
			"unit-1",         // name
			"ws-1",           // workspace
			"openai",         // provider
			"gpt-4",          // model
			"hello",          // prompt
			"",               // expected
			"heuristic",      // eval_method
			0.7,              // threshold
			[]string{"smoke"},
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	out, err := p.AddTestCase(context.Background(), TestCase{
		Name: "unit-1", WorkspaceID: "ws-1",
		Provider: "openai", Model: "gpt-4",
		Prompt: "hello", PassThreshold: 0.7,
		Tags: []string{"smoke"},
	})
	if err != nil {
		t.Fatalf("AddTestCase: %v", err)
	}
	if out.ID == "" {
		t.Error("ID should be set")
	}
	if out.EvalMethod != EvalHeuristic {
		t.Errorf("default EvalMethod = %q, want heuristic", out.EvalMethod)
	}
}

func TestRunTestCase_EvalHeuristicScoresResponse(t *testing.T) {
	srv := openAIMock(t, func(string) string { return "This is a thoughtful answer that elaborates on the question." })
	p := newPipeline(nil, quality.New(nil), "k", "k", "k")
	p.openAIURL = srv.URL

	res := p.RunTestCase(context.Background(), TestCase{
		Name: "h", Provider: "openai", Model: "gpt-4",
		Prompt: "Explain photosynthesis briefly.", EvalMethod: EvalHeuristic, PassThreshold: 0.0,
	})
	if res.Error != "" {
		t.Fatalf("error: %s", res.Error)
	}
	if res.Score <= 0 {
		t.Errorf("heuristic score = %v, want > 0 for a substantive response", res.Score)
	}
	if res.Response == "" {
		t.Error("Response should be captured in-memory on EvalResult")
	}
}

func TestRunTestCase_EvalExactPassesOnExactMatch(t *testing.T) {
	srv := openAIMock(t, func(string) string { return "yes" })
	p := newPipeline(nil, quality.New(nil), "k", "k", "k")
	p.openAIURL = srv.URL

	res := p.RunTestCase(context.Background(), TestCase{
		Name: "e", Provider: "openai", Model: "gpt-4",
		Prompt: "Is the sky blue?", ExpectedOutput: "yes",
		EvalMethod: EvalExact, PassThreshold: 1.0,
	})
	if res.Score != 1.0 {
		t.Errorf("Score = %v, want 1.0", res.Score)
	}
	if !res.Passed {
		t.Error("Passed = false, want true on exact match")
	}
}

func TestRunTestCase_EvalExactFailsOnMismatch(t *testing.T) {
	srv := openAIMock(t, func(string) string { return "nope" })
	p := newPipeline(nil, quality.New(nil), "k", "k", "k")
	p.openAIURL = srv.URL

	res := p.RunTestCase(context.Background(), TestCase{
		Name: "ef", Provider: "openai", Model: "gpt-4",
		Prompt: "Is the sky blue?", ExpectedOutput: "yes",
		EvalMethod: EvalExact, PassThreshold: 1.0,
	})
	if res.Score != 0.0 {
		t.Errorf("Score = %v, want 0.0", res.Score)
	}
	if res.Passed {
		t.Error("Passed = true, want false on mismatch")
	}
}

func TestRunTestCase_EvalContainsPassesWhenSubstringFound(t *testing.T) {
	srv := openAIMock(t, func(string) string { return "yes, the sky is generally blue during daytime." })
	p := newPipeline(nil, quality.New(nil), "k", "k", "k")
	p.openAIURL = srv.URL

	res := p.RunTestCase(context.Background(), TestCase{
		Name: "c", Provider: "openai", Model: "gpt-4",
		Prompt: "color of sky?", ExpectedOutput: "blue",
		EvalMethod: EvalContains, PassThreshold: 1.0,
	})
	if res.Score != 1.0 {
		t.Errorf("Score = %v, want 1.0", res.Score)
	}
	if !res.Passed {
		t.Error("Passed = false, want true when substring matches")
	}
}

func TestRunTestCase_EvalLLMJudgeParsesNumericScore(t *testing.T) {
	// Mock returns the test-case body for the first model, and a numeric
	// score for the judge model. judgeModel is the cheap default.
	srv := openAIMock(t, func(model string) string {
		if model == judgeModel {
			return "7"
		}
		return "Here is the answer being judged."
	})
	p := newPipeline(nil, quality.New(nil), "k", "k", "k")
	p.openAIURL = srv.URL

	res := p.RunTestCase(context.Background(), TestCase{
		Name: "j", Provider: "openai", Model: "gpt-4",
		Prompt: "anything", EvalMethod: EvalLLMJudge, PassThreshold: 0.5,
	})
	if res.Error != "" {
		t.Fatalf("judge errored: %s", res.Error)
	}
	if res.Score < 0.69 || res.Score > 0.71 {
		t.Errorf("Score = %v, want ~0.7 for judge returning 7", res.Score)
	}
	if !res.Passed {
		t.Error("Passed = false, want true when judge score 7/10 >= 0.5 threshold")
	}
}

func TestRunSuite_CollectsResultsAndComputesPassRate(t *testing.T) {
	pool := newPool(t)
	now := time.Now().UTC()
	// Two test cases — one will pass (exact match), one will fail (mismatch).
	pool.ExpectQuery(`FROM eval_test_cases`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "name", "workspace_id", "provider", "model",
			"prompt", "expected_output", "eval_method", "pass_threshold",
			"tags", "created_at",
		}).
			AddRow("tc-1", "passes", "ws-1", "openai", "gpt-4", "say-yes", "yes", "exact", 1.0, []string{}, now).
			AddRow("tc-2", "fails", "ws-1", "openai", "gpt-4", "say-yes", "yes", "exact", 1.0, []string{}, now))

	// Two INSERT eval_results + one INSERT eval_runs.
	pool.ExpectExec(`INSERT INTO eval_results`).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectExec(`INSERT INTO eval_results`).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectExec(`INSERT INTO eval_runs`).WillReturnResult(pgxmock.NewResult("INSERT", 1))

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Distinguish the two test cases by which one this hit corresponds to.
		// The order of execution isn't guaranteed (concurrent), so respond
		// with one of two answers in alternation.
		n := atomic.AddInt32(&hits, 1)
		content := "yes"
		if n%2 == 0 {
			content = "no"
		}
		_ = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"`+content+`"}}]}`)
	}))
	t.Cleanup(srv.Close)

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	p.openAIURL = srv.URL

	summary, err := p.RunSuite(context.Background(), "ws-1", nil)
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if summary.TotalTests != 2 {
		t.Errorf("TotalTests = %d, want 2", summary.TotalTests)
	}
	if summary.Passed+summary.Failed != 2 {
		t.Errorf("Passed+Failed = %d, want 2", summary.Passed+summary.Failed)
	}
	if summary.Passed != 1 || summary.Failed != 1 {
		t.Errorf("Passed=%d Failed=%d, want 1/1", summary.Passed, summary.Failed)
	}
	if summary.PassRate < 0.49 || summary.PassRate > 0.51 {
		t.Errorf("PassRate = %v, want ~0.5", summary.PassRate)
	}
	if summary.RunID == "" {
		t.Error("RunID should be set")
	}
}

func TestGetResults_ReturnsCorrectRunResults(t *testing.T) {
	pool := newPool(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM eval_results WHERE run_id = \$1`).
		WithArgs("run-xyz").
		WillReturnRows(pgxmock.NewRows([]string{
			"test_case_id", "run_id", "passed", "score",
			"latency_ms", "cost_usd", "eval_method", "error", "created_at",
		}).
			AddRow("tc-1", "run-xyz", true, 0.95, int64(120), 0.001, "heuristic", "", now).
			AddRow("tc-2", "run-xyz", false, 0.2, int64(95), 0.001, "exact", "", now))

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	results, err := p.GetResults(context.Background(), "run-xyz")
	if err != nil {
		t.Fatalf("GetResults: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].TestCaseID != "tc-1" || !results[0].Passed {
		t.Errorf("results[0] wrong: %+v", results[0])
	}
	if results[1].TestCaseID != "tc-2" || results[1].Passed {
		t.Errorf("results[1] wrong: %+v", results[1])
	}
}

// Sanity: package builds and unrelated symbols stay accessible.
func TestPackage_StringsPresent(t *testing.T) {
	if !strings.HasPrefix(string(EvalHeuristic), "heur") {
		t.Errorf("EvalHeuristic constant has unexpected value %q", EvalHeuristic)
	}
}
