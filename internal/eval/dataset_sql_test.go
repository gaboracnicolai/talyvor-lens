package eval

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/quality"
)

// These tests exercise the hand-written dataset/schedule SQL against pgxmock so
// column/placeholder/scan-order mismatches — the bug class that passes the
// build and review but fails at runtime — are caught. They mirror the existing
// AddTestCase pgxmock pattern.

func TestCreateDataset_Inserts(t *testing.T) {
	pool := newPool(t)
	pool.ExpectExec(`INSERT INTO eval_datasets`).
		WithArgs(pgxmock.AnyArg(), "ws1", "golden", "the desc", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	ds, err := p.CreateDataset(context.Background(), Dataset{WorkspaceID: "ws1", Name: "golden", Description: "the desc"})
	if err != nil {
		t.Fatalf("CreateDataset: %v", err)
	}
	if ds.ID == "" || ds.WorkspaceID != "ws1" {
		t.Errorf("dataset = %+v", ds)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestListDatasets_ParsesRows(t *testing.T) {
	pool := newPool(t)
	rows := pgxmock.NewRows([]string{"id", "workspace_id", "name", "description", "created_at"}).
		AddRow("d1", "ws1", "golden", "desc", time.Now())
	pool.ExpectQuery(`FROM eval_datasets WHERE workspace_id`).WithArgs("ws1").WillReturnRows(rows)

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	list, err := p.ListDatasets(context.Background(), "ws1")
	if err != nil {
		t.Fatalf("ListDatasets: %v", err)
	}
	if len(list) != 1 || list[0].ID != "d1" || list[0].Name != "golden" {
		t.Errorf("list = %+v", list)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestGetDataset_ScansRow(t *testing.T) {
	pool := newPool(t)
	rows := pgxmock.NewRows([]string{"id", "workspace_id", "name", "description", "created_at"}).
		AddRow("d1", "ws1", "golden", "desc", time.Now())
	pool.ExpectQuery(`FROM eval_datasets WHERE id`).WithArgs("d1", "ws1").WillReturnRows(rows)

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	ds, err := p.GetDataset(context.Background(), "ws1", "d1")
	if err != nil {
		t.Fatalf("GetDataset: %v", err)
	}
	if ds.ID != "d1" || ds.WorkspaceID != "ws1" {
		t.Errorf("dataset = %+v", ds)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAddDatasetCase_InsertsWithDatasetID(t *testing.T) {
	pool := newPool(t)
	pool.ExpectExec(`INSERT INTO eval_test_cases`).
		WithArgs(
			pgxmock.AnyArg(), // id
			"case-a",         // name
			"ws1",            // workspace_id
			"ds1",            // dataset_id
			"openai",         // provider
			"gpt-4o",         // model
			"prompt",         // prompt
			"exp",            // expected_output
			"contains",       // eval_method
			1.0,              // pass_threshold
			[]string{"tag"},  // tags
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	tc, err := p.AddDatasetCase(context.Background(), "ds1", TestCase{
		Name: "case-a", WorkspaceID: "ws1", Provider: "openai", Model: "gpt-4o",
		Prompt: "prompt", ExpectedOutput: "exp", EvalMethod: EvalContains,
		PassThreshold: 1.0, Tags: []string{"tag"},
	})
	if err != nil {
		t.Fatalf("AddDatasetCase: %v", err)
	}
	if tc.DatasetID != "ds1" {
		t.Errorf("dataset_id = %q, want ds1", tc.DatasetID)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestListDatasetCases_ParsesRows(t *testing.T) {
	pool := newPool(t)
	rows := pgxmock.NewRows([]string{
		"id", "name", "workspace_id", "dataset_id", "provider", "model",
		"prompt", "expected_output", "eval_method", "pass_threshold", "tags", "created_at",
	}).AddRow("c1", "case", "ws1", "ds1", "openai", "gpt-4o", "p", "e", "contains", 1.0, []string{"t"}, time.Now())
	pool.ExpectQuery(`FROM eval_test_cases WHERE dataset_id`).WithArgs("ds1").WillReturnRows(rows)

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	cases, err := p.ListDatasetCases(context.Background(), "ds1")
	if err != nil {
		t.Fatalf("ListDatasetCases: %v", err)
	}
	if len(cases) != 1 || cases[0].ID != "c1" || cases[0].DatasetID != "ds1" || cases[0].EvalMethod != EvalContains {
		t.Errorf("cases = %+v", cases)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestCreateSchedule_Inserts(t *testing.T) {
	pool := newPool(t)
	pool.ExpectExec(`INSERT INTO eval_schedules`).
		WithArgs(pgxmock.AnyArg(), "ws1", "ds1", 3600, true, "gpt-4o-mini", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	s, err := p.CreateSchedule(context.Background(), Schedule{
		WorkspaceID: "ws1", DatasetID: "ds1", IntervalSec: 3600, Enabled: true, TargetModel: "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if s.ID == "" || s.DatasetID != "ds1" {
		t.Errorf("schedule = %+v", s)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestListSchedules_ParsesRows(t *testing.T) {
	pool := newPool(t)
	rows := pgxmock.NewRows([]string{
		"id", "workspace_id", "dataset_id", "interval_seconds", "enabled", "target_model", "last_run_at", "created_at",
	}).AddRow("s1", "ws1", "ds1", 3600, true, "gpt-4o-mini", nil, time.Now())
	pool.ExpectQuery(`FROM eval_schedules WHERE workspace_id`).WithArgs("ws1").WillReturnRows(rows)

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	list, err := p.ListSchedules(context.Background(), "ws1")
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(list) != 1 || list[0].ID != "s1" || list[0].IntervalSec != 3600 || !list[0].Enabled {
		t.Errorf("schedules = %+v", list)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// The integration that ties the new SQL together: RunEval loads the dataset
// cases, finds the prior run, loads its per-case results as the baseline, runs
// the cases, persists, and detects a regression vs the baseline — all through
// the real queries.
func TestRunEval_LoadsBaselineAndDetectsRegression(t *testing.T) {
	srv := openAIMock(t, func(string) string { return "nope" }) // won't contain "MATCHME"
	pool := newPool(t)

	caseRows := pgxmock.NewRows([]string{
		"id", "name", "workspace_id", "dataset_id", "provider", "model",
		"prompt", "expected_output", "eval_method", "pass_threshold", "tags", "created_at",
	}).AddRow("case-1", "c", "ws1", "ds1", "openai", "gpt-4o", "q", "MATCHME", "contains", 1.0, []string{}, time.Now())
	pool.ExpectQuery(`FROM eval_test_cases WHERE dataset_id`).WithArgs("ds1").WillReturnRows(caseRows)

	// Prior run lookup → "run-prev".
	pool.ExpectQuery(`SELECT id FROM eval_runs`).WithArgs("ds1").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("run-prev"))

	// Baseline results for run-prev: case-1 previously scored 0.9 and passed.
	baseRows := pgxmock.NewRows([]string{
		"test_case_id", "run_id", "passed", "score", "latency_ms", "cost_usd", "eval_method", "error", "created_at",
	}).AddRow("case-1", "run-prev", true, 0.9, int64(10), 0.0, "contains", "", time.Now())
	pool.ExpectQuery(`FROM eval_results WHERE run_id`).WithArgs("run-prev").WillReturnRows(baseRows)

	// Current run persistence: one result insert + one run insert.
	pool.ExpectExec(`INSERT INTO eval_results`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectExec(`INSERT INTO eval_runs`).
		WithArgs(pgxmock.AnyArg(), "ws1", "ds1", pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	p := newPipeline(pool, quality.New(nil), "k", "k", "k")
	p.openAIURL = srv.URL

	run, err := p.RunEval(context.Background(), "ws1", "ds1", Target{})
	if err != nil {
		t.Fatalf("RunEval: %v", err)
	}
	if run.BaselineRunID != "run-prev" {
		t.Errorf("baseline = %q, want run-prev", run.BaselineRunID)
	}
	if len(run.Regressions) != 1 {
		t.Fatalf("want 1 regression (0.9→0, pass→fail), got %+v", run.Regressions)
	}
	if run.Regressions[0].TestCaseID != "case-1" || run.Regressions[0].Delta >= 0 {
		t.Errorf("regression = %+v", run.Regressions[0])
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
