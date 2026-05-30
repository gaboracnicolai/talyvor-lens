// Package eval runs a test-case suite against the live providers,
// scores the responses with one of four methods (heuristic, exact,
// contains, LLM-as-judge), and records pass/fail history so regressions
// surface in CI before users see them. RunSuite is bounded to ten
// concurrent test cases; LLM-judge calls always use the cheapest
// gpt-4o-mini model so the eval budget stays predictable.
package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/quality"
)

const (
	// judgeModel is the cheapest OpenAI model — keeps eval suite costs
	// bounded even when a suite runs every commit.
	judgeModel = "gpt-4o-mini"

	// maxConcurrent caps RunSuite's parallelism. Hard-coded per spec;
	// a 10-test simultaneous burst is enough to keep latency low without
	// flooding the upstream provider's rate limits.
	maxConcurrent = 10

	// perTestTimeout bounds each LLM call. Eval suites that hang on a
	// single test case used to block CI — this guarantees the worst case.
	perTestTimeout = 30 * time.Second

	// passRateAlertThreshold drives the "regression alert" log line at
	// the end of every scheduled run.
	passRateAlertThreshold = 0.8
)

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type EvalMethod string

const (
	EvalHeuristic EvalMethod = "heuristic"
	EvalExact     EvalMethod = "exact"
	EvalContains  EvalMethod = "contains"
	EvalLLMJudge  EvalMethod = "llm_judge"
)

type TestCase struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	WorkspaceID    string     `json:"workspace_id"`
	DatasetID      string     `json:"dataset_id,omitempty"`
	Provider       string     `json:"provider"`
	Model          string     `json:"model"`
	Prompt         string     `json:"prompt"`
	ExpectedOutput string     `json:"expected_output"`
	EvalMethod     EvalMethod `json:"eval_method"`
	PassThreshold  float64    `json:"pass_threshold"`
	Tags           []string   `json:"tags"`
	CreatedAt      time.Time  `json:"created_at"`
}

type EvalResult struct {
	TestCaseID string     `json:"test_case_id"`
	TestName   string     `json:"test_name"`
	RunID      string     `json:"run_id"`
	Passed     bool       `json:"passed"`
	Score      float64    `json:"score"`
	Response   string     `json:"response"` // memory-only — never persisted
	EvalMethod EvalMethod `json:"eval_method"`
	LatencyMs  int64      `json:"latency_ms"`
	CostUSD    float64    `json:"cost_usd"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

type RunSummary struct {
	RunID        string     `json:"run_id"`
	WorkspaceID  string     `json:"workspace_id"`
	TotalTests   int        `json:"total_tests"`
	Passed       int        `json:"passed"`
	Failed       int        `json:"failed"`
	PassRate     float64    `json:"pass_rate"`
	TotalCostUSD float64    `json:"total_cost_usd"`
	AvgLatencyMs int64      `json:"avg_latency_ms"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

type Pipeline struct {
	pool         pgxDB
	scorer       *quality.Scorer
	httpClient   *http.Client
	spend        SpendRecorder // optional eval-spend attribution (nil = off)
	openAIKey    string
	anthropicKey string
	googleKey    string

	// Per-provider URL overrides for tests. Defaults point at the real
	// upstream services; tests swap these for httptest server URLs.
	openAIURL    string
	anthropicURL string
	googleURL    string
}

func New(pool *pgxpool.Pool, scorer *quality.Scorer, openAIKey, anthropicKey, googleKey string) *Pipeline {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newPipeline(db, scorer, openAIKey, anthropicKey, googleKey)
}

func newPipeline(pool pgxDB, scorer *quality.Scorer, openAIKey, anthropicKey, googleKey string) *Pipeline {
	return &Pipeline{
		pool:         pool,
		scorer:       scorer,
		httpClient:   &http.Client{Timeout: perTestTimeout + 5*time.Second},
		openAIKey:    openAIKey,
		anthropicKey: anthropicKey,
		googleKey:    googleKey,
		openAIURL:    "https://api.openai.com/v1/chat/completions",
		anthropicURL: "https://api.anthropic.com/v1/messages",
		googleURL:    "https://generativelanguage.googleapis.com",
	}
}

const insertTestCaseSQL = `INSERT INTO eval_test_cases
    (id, name, workspace_id, provider, model, prompt, expected_output, eval_method, pass_threshold, tags)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

func (p *Pipeline) AddTestCase(ctx context.Context, tc TestCase) (*TestCase, error) {
	if strings.TrimSpace(tc.Name) == "" {
		return nil, errors.New("eval: Name required")
	}
	if strings.TrimSpace(tc.Prompt) == "" {
		return nil, errors.New("eval: Prompt required")
	}
	if tc.PassThreshold < 0 || tc.PassThreshold > 1 {
		return nil, fmt.Errorf("eval: PassThreshold %v outside [0,1]", tc.PassThreshold)
	}
	if tc.Provider == "" || tc.Model == "" {
		return nil, errors.New("eval: Provider and Model required")
	}
	if tc.EvalMethod == "" {
		tc.EvalMethod = EvalHeuristic
	}
	if tc.WorkspaceID == "" {
		tc.WorkspaceID = "default"
	}
	if tc.ID == "" {
		tc.ID = uuid.NewString()
	}
	if tc.Tags == nil {
		tc.Tags = []string{}
	}
	tc.CreatedAt = time.Now().UTC()

	if p.pool != nil {
		if _, err := p.pool.Exec(ctx, insertTestCaseSQL,
			tc.ID, tc.Name, tc.WorkspaceID, tc.Provider, tc.Model,
			tc.Prompt, tc.ExpectedOutput, string(tc.EvalMethod), tc.PassThreshold, tc.Tags,
		); err != nil {
			return nil, fmt.Errorf("eval: insert test case: %w", err)
		}
	}
	return &tc, nil
}

// RunTestCase executes one test case against the configured LLM and
// scores the response. Public callers get a fresh runID; RunSuite uses
// runTestCaseWith to share a runID across the suite's results.
func (p *Pipeline) RunTestCase(ctx context.Context, tc TestCase) EvalResult {
	return p.runTestCaseWith(ctx, tc, uuid.NewString())
}

func (p *Pipeline) runTestCaseWith(ctx context.Context, tc TestCase, runID string) EvalResult {
	if tc.EvalMethod == "" {
		tc.EvalMethod = EvalHeuristic
	}
	result := EvalResult{
		TestCaseID: tc.ID, TestName: tc.Name, RunID: runID,
		EvalMethod: tc.EvalMethod, CreatedAt: time.Now().UTC(),
	}

	callCtx, cancel := context.WithTimeout(ctx, perTestTimeout)
	defer cancel()

	started := time.Now()
	response, err := p.callLLM(callCtx, tc.Provider, tc.Model, tc.Prompt)
	result.LatencyMs = time.Since(started).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Response = response
	result.CostUSD = alerts.CostUSD(tc.Model, len(tc.Prompt)/4, len(response)/4)

	// Deterministic, network-free methods (exact/contains/regex/json_schema)
	// are scored by staticScore. A configuration error (bad regex/schema) is
	// surfaced on the result rather than silently scored 0.
	if s, handled, serr := staticScore(tc.EvalMethod, tc.ExpectedOutput, response); handled {
		if serr != nil {
			result.Error = serr.Error()
			return result
		}
		result.Score = s
	} else {
		switch tc.EvalMethod {
		case EvalLLMJudge:
			score, jerr := p.judgeResponse(callCtx, tc.Prompt, response)
			if jerr != nil {
				result.Error = jerr.Error()
				return result
			}
			result.Score = score
		default:
			q := p.scorer.ScoreResponse(callCtx, tc.Prompt, response, tc.Provider, tc.Model)
			result.Score = q.Score
		}
	}

	result.Passed = result.Score >= tc.PassThreshold
	return result
}

const insertResultSQL = `INSERT INTO eval_results
    (test_case_id, run_id, passed, score, latency_ms, cost_usd, eval_method, error)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

const insertRunSQL = `INSERT INTO eval_runs
    (id, workspace_id, total_tests, passed, failed, pass_rate, total_cost_usd, avg_latency_ms, completed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

const selectTestCasesSQL = `SELECT id, name, workspace_id, provider, model,
    prompt, expected_output, eval_method, pass_threshold, tags, created_at
FROM eval_test_cases WHERE workspace_id = $1`

const selectResultsSQL = `SELECT test_case_id, run_id, passed, score,
    latency_ms, cost_usd, eval_method, error, created_at
FROM eval_results WHERE run_id = $1 ORDER BY created_at ASC`

const selectRunSQL = `SELECT id, workspace_id, total_tests, passed, failed,
    pass_rate, total_cost_usd, avg_latency_ms, created_at, completed_at
FROM eval_runs WHERE id = $1`

const selectRunsSQL = `SELECT id, workspace_id, total_tests, passed, failed,
    pass_rate, total_cost_usd, avg_latency_ms, created_at, completed_at
FROM eval_runs WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT $2`

func (p *Pipeline) ListTestCases(ctx context.Context, workspaceID string, tags []string) ([]TestCase, error) {
	if p.pool == nil {
		return nil, nil
	}
	if workspaceID == "" {
		workspaceID = "default"
	}
	rows, err := p.pool.Query(ctx, selectTestCasesSQL, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TestCase
	for rows.Next() {
		var tc TestCase
		var method string
		if err := rows.Scan(&tc.ID, &tc.Name, &tc.WorkspaceID, &tc.Provider, &tc.Model,
			&tc.Prompt, &tc.ExpectedOutput, &method, &tc.PassThreshold, &tc.Tags, &tc.CreatedAt); err != nil {
			return nil, err
		}
		tc.EvalMethod = EvalMethod(method)
		if matchesTags(tc.Tags, tags) {
			out = append(out, tc)
		}
	}
	return out, rows.Err()
}

func matchesTags(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(have))
	for _, t := range have {
		set[t] = struct{}{}
	}
	for _, t := range want {
		if _, ok := set[t]; !ok {
			return false
		}
	}
	return true
}

// RunSuite walks every test case in the workspace (optionally filtered
// by tags) up to maxConcurrent at a time and persists results + an
// aggregate run summary. Response text is held in memory only; the DB
// rows store metadata exclusively.
func (p *Pipeline) RunSuite(ctx context.Context, workspaceID string, tags []string) (*RunSummary, error) {
	cases, err := p.ListTestCases(ctx, workspaceID, tags)
	if err != nil {
		return nil, err
	}
	runID := uuid.NewString()
	start := time.Now().UTC()
	sem := make(chan struct{}, maxConcurrent)
	results := make([]EvalResult, len(cases))
	var wg sync.WaitGroup
	for i, tc := range cases {
		i, tc := i, tc
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = p.runTestCaseWith(ctx, tc, runID)
		}()
	}
	wg.Wait()

	summary := &RunSummary{
		RunID:       runID,
		WorkspaceID: workspaceID,
		TotalTests:  len(results),
		CreatedAt:   start,
	}
	var (
		totalLatency int64
		totalCost    float64
	)
	for _, res := range results {
		if res.Passed {
			summary.Passed++
		} else {
			summary.Failed++
		}
		totalLatency += res.LatencyMs
		totalCost += res.CostUSD
		if p.pool != nil {
			_, _ = p.pool.Exec(ctx, insertResultSQL,
				res.TestCaseID, res.RunID, res.Passed, res.Score,
				res.LatencyMs, res.CostUSD, string(res.EvalMethod), res.Error,
			)
		}
	}
	if summary.TotalTests > 0 {
		summary.PassRate = float64(summary.Passed) / float64(summary.TotalTests)
		summary.AvgLatencyMs = totalLatency / int64(summary.TotalTests)
	}
	summary.TotalCostUSD = totalCost
	completed := time.Now().UTC()
	summary.CompletedAt = &completed

	if p.pool != nil {
		_, _ = p.pool.Exec(ctx, insertRunSQL,
			summary.RunID, summary.WorkspaceID, summary.TotalTests,
			summary.Passed, summary.Failed, summary.PassRate,
			summary.TotalCostUSD, summary.AvgLatencyMs, completed,
		)
	}
	return summary, nil
}

func (p *Pipeline) GetResults(ctx context.Context, runID string) ([]EvalResult, error) {
	if p.pool == nil {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, selectResultsSQL, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvalResult
	for rows.Next() {
		var r EvalResult
		var method string
		if err := rows.Scan(&r.TestCaseID, &r.RunID, &r.Passed, &r.Score,
			&r.LatencyMs, &r.CostUSD, &method, &r.Error, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.EvalMethod = EvalMethod(method)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Pipeline) GetRun(ctx context.Context, runID string) (*RunSummary, error) {
	if p.pool == nil {
		return nil, errors.New("eval: no pool")
	}
	var s RunSummary
	err := p.pool.QueryRow(ctx, selectRunSQL, runID).Scan(
		&s.RunID, &s.WorkspaceID, &s.TotalTests, &s.Passed, &s.Failed,
		&s.PassRate, &s.TotalCostUSD, &s.AvgLatencyMs, &s.CreatedAt, &s.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (p *Pipeline) ListRuns(ctx context.Context, workspaceID string, limit int) ([]RunSummary, error) {
	if p.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := p.pool.Query(ctx, selectRunsSQL, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunSummary
	for rows.Next() {
		var s RunSummary
		if err := rows.Scan(&s.RunID, &s.WorkspaceID, &s.TotalTests, &s.Passed, &s.Failed,
			&s.PassRate, &s.TotalCostUSD, &s.AvgLatencyMs, &s.CreatedAt, &s.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ScheduleRun is a long-lived loop that triggers RunSuite at the given
// interval. It logs a CRITICAL-level message whenever the suite pass
// rate drops below 80% so alerting (Slack, PagerDuty) can pick it up
// off the structured log stream. Exits on ctx.Done().
func (p *Pipeline) ScheduleRun(ctx context.Context, workspaceID string, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			summary, err := p.RunSuite(ctx, workspaceID, nil)
			if err != nil {
				slog.Error("eval: scheduled run failed",
					slog.String("workspace_id", workspaceID),
					slog.String("err", err.Error()),
				)
				continue
			}
			if summary.PassRate < passRateAlertThreshold && summary.TotalTests > 0 {
				slog.Error("eval: pass rate below threshold",
					slog.String("workspace_id", workspaceID),
					slog.String("run_id", summary.RunID),
					slog.Float64("pass_rate", summary.PassRate),
					slog.Int("passed", summary.Passed),
					slog.Int("total", summary.TotalTests),
					slog.String("alert", "Eval suite pass rate below 80%"),
				)
			}
		}
	}
}

// callLLM POSTs a single-message chat request to the configured
// provider URL and returns the assistant content. Per-provider wire
// formats are handled inline — eval doesn't need the proxy's full
// cache/route/score pipeline, just the upstream call.
func (p *Pipeline) callLLM(ctx context.Context, provider, model, prompt string) (string, error) {
	switch provider {
	case "openai":
		return p.callOpenAI(ctx, model, prompt)
	case "anthropic":
		return p.callAnthropic(ctx, model, prompt)
	case "google":
		return p.callGoogle(ctx, model, prompt)
	default:
		return "", fmt.Errorf("eval: unsupported provider %q", provider)
	}
}

func (p *Pipeline) callOpenAI(ctx context.Context, model, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.openAIURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.openAIKey)
	return doAndExtractOpenAI(p.httpClient, req)
}

func (p *Pipeline) callAnthropic(ctx context.Context, model, prompt string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1024,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.anthropicURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.anthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("anthropic: status %d", resp.StatusCode)
	}
	// Anthropic shape: {"content":[{"type":"text","text":"..."}]}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, c := range out.Content {
		sb.WriteString(c.Text)
	}
	return sb.String(), nil
}

func (p *Pipeline) callGoogle(ctx context.Context, model, prompt string) (string, error) {
	u := p.googleURL + "/v1beta/models/" + model + ":generateContent?key=" + url.QueryEscape(p.googleKey)
	body, _ := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]string{{"text": prompt}}},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("google: status %d", resp.StatusCode)
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if len(out.Candidates) == 0 {
		return "", nil
	}
	var sb strings.Builder
	for _, part := range out.Candidates[0].Content.Parts {
		sb.WriteString(part.Text)
	}
	return sb.String(), nil
}

// doAndExtractOpenAI runs the request and returns the first assistant
// message's content. Shared by callOpenAI and judgeResponse.
func doAndExtractOpenAI(client *http.Client, req *http.Request) (string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("openai: status %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", nil
	}
	return out.Choices[0].Message.Content, nil
}

// judgeResponse asks the cheap judge model to rate a response 0-10 and
// converts the parsed integer to a 0..1 score. Anything that doesn't
// look like a number returns an error; the test result records that as
// a failure rather than silently scoring zero.
func (p *Pipeline) judgeResponse(ctx context.Context, prompt, response string) (float64, error) {
	judgePrompt := "Rate this response 0-10 for quality and relevance. Return only a single integer.\n\nResponse:\n" + response
	body, _ := json.Marshal(map[string]any{
		"model": judgeModel,
		"messages": []map[string]string{
			{"role": "user", "content": judgePrompt},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.openAIURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.openAIKey)
	out, err := doAndExtractOpenAI(p.httpClient, req)
	if err != nil {
		return 0, err
	}
	// Tolerant parse: extract the first integer 0-10 in the response.
	score, perr := parseJudgeScore(out)
	if perr != nil {
		return 0, perr
	}
	return float64(score) / 10.0, nil
}

func parseJudgeScore(out string) (int, error) {
	out = strings.TrimSpace(out)
	// Walk the string to find the first run of digits.
	for i := 0; i < len(out); i++ {
		if out[i] >= '0' && out[i] <= '9' {
			j := i
			for j < len(out) && out[j] >= '0' && out[j] <= '9' {
				j++
			}
			n, err := strconv.Atoi(out[i:j])
			if err != nil {
				return 0, err
			}
			if n < 0 {
				n = 0
			}
			if n > 10 {
				n = 10
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("eval: judge returned no parseable score: %q", out)
}
