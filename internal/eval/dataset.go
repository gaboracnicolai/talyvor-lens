package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/metrics"
)

// SpendRecorder attributes eval spend through the normal cost ledger so an eval
// run shows up in budgets/forecasts/reports like any other usage. It matches
// alerts.AlertManager.RecordSpend, which satisfies this interface directly. nil
// (the default) disables attribution — handy for tests and dry runs.
type SpendRecorder interface {
	RecordSpend(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool) error
}

// SetSpendRecorder wires eval-spend attribution. Call with the AlertManager so
// eval calls land in token_events tagged feature/modality "eval".
func (p *Pipeline) SetSpendRecorder(r SpendRecorder) { p.spend = r }

// Dataset is a named, per-workspace collection of test cases — the golden set a
// model/prompt change is regression-tested against.
type Dataset struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// EvalRun is the result of running a dataset against a target: the aggregate
// summary, per-case results, the cost estimate, an aggregate pass/fail vs the
// threshold, and any regressions vs the prior run on the same dataset.
type EvalRun struct {
	Summary       RunSummary   `json:"summary"`
	DatasetID     string       `json:"dataset_id,omitempty"`
	Target        Target       `json:"target"`
	Estimate      CostEstimate `json:"estimate"`
	Results       []EvalResult `json:"results"`
	Regressions   []Regression `json:"regressions"`
	BaselineRunID string       `json:"baseline_run_id,omitempty"`
	PassThreshold float64      `json:"pass_threshold"`
	Passed        bool         `json:"passed"`
}

const (
	insertDatasetSQL = `INSERT INTO eval_datasets (id, workspace_id, name, description, created_at)
VALUES ($1, $2, $3, $4, $5)`

	selectDatasetsSQL = `SELECT id, workspace_id, name, description, created_at
FROM eval_datasets WHERE workspace_id = $1 ORDER BY created_at DESC`

	selectDatasetSQL = `SELECT id, workspace_id, name, description, created_at
FROM eval_datasets WHERE id = $1 AND workspace_id = $2`

	insertDatasetCaseSQL = `INSERT INTO eval_test_cases
    (id, name, workspace_id, dataset_id, provider, model, prompt, expected_output, eval_method, pass_threshold, tags)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	selectDatasetCasesSQL = `SELECT id, name, workspace_id, dataset_id, provider, model,
    prompt, expected_output, eval_method, pass_threshold, tags, created_at
FROM eval_test_cases WHERE dataset_id = $1 ORDER BY created_at ASC`

	insertDatasetRunSQL = `INSERT INTO eval_runs
    (id, workspace_id, dataset_id, total_tests, passed, failed, pass_rate, total_cost_usd, avg_latency_ms, completed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	latestDatasetRunSQL = `SELECT id FROM eval_runs
WHERE dataset_id = $1 ORDER BY created_at DESC LIMIT 1`
)

// CreateDataset validates and persists a new dataset.
func (p *Pipeline) CreateDataset(ctx context.Context, ds Dataset) (*Dataset, error) {
	if strings.TrimSpace(ds.Name) == "" {
		return nil, errors.New("eval: dataset Name required")
	}
	if ds.WorkspaceID == "" {
		ds.WorkspaceID = "default"
	}
	if ds.ID == "" {
		ds.ID = uuid.NewString()
	}
	ds.CreatedAt = time.Now().UTC()
	if p.pool != nil {
		if _, err := p.pool.Exec(ctx, insertDatasetSQL,
			ds.ID, ds.WorkspaceID, ds.Name, ds.Description, ds.CreatedAt); err != nil {
			return nil, fmt.Errorf("eval: insert dataset: %w", err)
		}
	}
	return &ds, nil
}

// ListDatasets returns every dataset in a workspace, newest first.
func (p *Pipeline) ListDatasets(ctx context.Context, workspaceID string) ([]Dataset, error) {
	if p.pool == nil {
		return nil, nil
	}
	if workspaceID == "" {
		workspaceID = "default"
	}
	rows, err := p.pool.Query(ctx, selectDatasetsSQL, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dataset
	for rows.Next() {
		var d Dataset
		if err := rows.Scan(&d.ID, &d.WorkspaceID, &d.Name, &d.Description, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDataset fetches one dataset scoped to its workspace.
func (p *Pipeline) GetDataset(ctx context.Context, workspaceID, id string) (*Dataset, error) {
	if p.pool == nil {
		return nil, errors.New("eval: no pool")
	}
	var d Dataset
	err := p.pool.QueryRow(ctx, selectDatasetSQL, id, workspaceID).Scan(
		&d.ID, &d.WorkspaceID, &d.Name, &d.Description, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// AddDatasetCase adds a test case scoped to a dataset. It mirrors AddTestCase's
// validation but persists the dataset_id linkage; the legacy AddTestCase path
// (workspace-level, no dataset) is unchanged.
func (p *Pipeline) AddDatasetCase(ctx context.Context, datasetID string, tc TestCase) (*TestCase, error) {
	if strings.TrimSpace(datasetID) == "" {
		return nil, errors.New("eval: datasetID required")
	}
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
	tc.DatasetID = datasetID
	tc.CreatedAt = time.Now().UTC()
	if p.pool != nil {
		if _, err := p.pool.Exec(ctx, insertDatasetCaseSQL,
			tc.ID, tc.Name, tc.WorkspaceID, tc.DatasetID, tc.Provider, tc.Model,
			tc.Prompt, tc.ExpectedOutput, string(tc.EvalMethod), tc.PassThreshold, tc.Tags,
		); err != nil {
			return nil, fmt.Errorf("eval: insert dataset case: %w", err)
		}
	}
	return &tc, nil
}

// ListDatasetCases returns the cases belonging to a dataset.
func (p *Pipeline) ListDatasetCases(ctx context.Context, datasetID string) ([]TestCase, error) {
	if p.pool == nil {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, selectDatasetCasesSQL, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TestCase
	for rows.Next() {
		var tc TestCase
		var method string
		if err := rows.Scan(&tc.ID, &tc.Name, &tc.WorkspaceID, &tc.DatasetID, &tc.Provider, &tc.Model,
			&tc.Prompt, &tc.ExpectedOutput, &method, &tc.PassThreshold, &tc.Tags, &tc.CreatedAt); err != nil {
			return nil, err
		}
		tc.EvalMethod = EvalMethod(method)
		out = append(out, tc)
	}
	return out, rows.Err()
}

// RunCases executes an explicit set of cases against a target and returns an
// EvalRun (cost estimate, per-case results, aggregate pass/fail). It enforces
// the target's cost cap BEFORE any model call and attributes spend through the
// configured recorder. This is the dataset-agnostic primitive; RunEval adds
// dataset loading + regression-vs-prior.
func (p *Pipeline) RunCases(ctx context.Context, workspaceID string, cases []TestCase, target Target) (*EvalRun, error) {
	return p.runCasesInternal(ctx, workspaceID, "", cases, target)
}

func (p *Pipeline) runCasesInternal(ctx context.Context, workspaceID, datasetID string, cases []TestCase, target Target) (*EvalRun, error) {
	est := estimateCost(cases, target)
	if err := checkCostCap(est, target.MaxCostUSD); err != nil {
		return nil, err
	}

	runID := uuid.NewString()
	start := time.Now().UTC()
	results := make([]EvalResult, len(cases))
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for i, tc := range cases {
		i, tc := i, tc
		if target.Model != "" {
			tc.Model = target.Model
		}
		if target.Provider != "" {
			tc.Provider = target.Provider
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = p.runTestCaseWith(ctx, tc, runID)
		}()
	}
	wg.Wait()

	summary := RunSummary{RunID: runID, WorkspaceID: workspaceID, TotalTests: len(results), CreatedAt: start}
	var totalLatency int64
	var totalCost float64
	for idx, res := range results {
		if res.Passed {
			summary.Passed++
		} else {
			summary.Failed++
		}
		totalLatency += res.LatencyMs
		totalCost += res.CostUSD
		// Attribute eval spend through the normal cost path (off the hot
		// path): tagged feature/modality "eval", estimated=true since token
		// counts are length approximations.
		if p.spend != nil && res.Error == "" {
			model := target.modelFor(cases[idx])
			_ = p.spend.RecordSpend(ctx, workspaceID, "", "", "eval", model,
				len(cases[idx].Prompt)/4, len(res.Response)/4,
				cases[idx].Prompt, "", runID, "eval", true)
		}
		if p.pool != nil {
			_, _ = p.pool.Exec(ctx, insertResultSQL,
				res.TestCaseID, res.RunID, res.Passed, res.Score,
				res.LatencyMs, res.CostUSD, string(res.EvalMethod), res.Error)
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
		_, _ = p.pool.Exec(ctx, insertDatasetRunSQL,
			summary.RunID, summary.WorkspaceID, datasetID, summary.TotalTests,
			summary.Passed, summary.Failed, summary.PassRate,
			summary.TotalCostUSD, summary.AvgLatencyMs, completed)
	}

	run := &EvalRun{
		Summary:       summary,
		DatasetID:     datasetID,
		Target:        target,
		Estimate:      est,
		Results:       results,
		PassThreshold: passRateAlertThreshold,
		Passed:        summary.PassRate >= passRateAlertThreshold && summary.TotalTests > 0,
	}
	if run.Passed {
		metrics.EvalRunRecorded("pass")
	} else {
		metrics.EvalRunRecorded("fail")
	}
	return run, nil
}

// RunEval runs an entire dataset against a target and detects regressions
// versus the most recent prior run on the same dataset.
func (p *Pipeline) RunEval(ctx context.Context, workspaceID, datasetID string, target Target) (*EvalRun, error) {
	cases, err := p.ListDatasetCases(ctx, datasetID)
	if err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("eval: dataset %q has no cases", datasetID)
	}

	// Capture the baseline (prior run's per-case results) BEFORE we add ours.
	var baselineResults []EvalResult
	var baselineRunID string
	if p.pool != nil {
		if id, lerr := p.latestDatasetRunID(ctx, datasetID); lerr == nil && id != "" {
			baselineRunID = id
			baselineResults, _ = p.GetResults(ctx, id)
		}
	}

	run, err := p.runCasesInternal(ctx, workspaceID, datasetID, cases, target)
	if err != nil {
		return nil, err
	}
	run.BaselineRunID = baselineRunID
	if len(baselineResults) > 0 {
		run.Regressions = detectRegressions(baselineResults, run.Results, RegressionEpsilon)
		metrics.EvalRegressionsDetected(len(run.Regressions))
	}
	return run, nil
}

func (p *Pipeline) latestDatasetRunID(ctx context.Context, datasetID string) (string, error) {
	var id string
	if err := p.pool.QueryRow(ctx, latestDatasetRunSQL, datasetID).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}
