package attribution

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestExtractAttribution_AllHeaders(t *testing.T) {
	tr := New(nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", nil)
	req.Header.Set("X-Talyvor-Branch", "feature/llm-cache")
	req.Header.Set("X-Talyvor-PR", "1234")
	req.Header.Set("X-Talyvor-Commit", "deadbeef")
	req.Header.Set("X-Talyvor-Team", "platform")
	req.Header.Set("X-Talyvor-Feature", "search")
	req.Header.Set("X-Talyvor-Repository", "acme/lens")

	got := tr.ExtractAttribution(req)

	want := Attribution{
		Branch:     "feature/llm-cache",
		PRNumber:   "1234",
		CommitSHA:  "deadbeef",
		Team:       "platform",
		Feature:    "search",
		Repository: "acme/lens",
	}
	if got != want {
		t.Errorf("ExtractAttribution = %+v, want %+v", got, want)
	}
}

func TestExtractAttribution_NoHeaders(t *testing.T) {
	tr := New(nil)
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	got := tr.ExtractAttribution(req)
	if got != (Attribution{}) {
		t.Errorf("ExtractAttribution = %+v, want zero value", got)
	}
}

func TestRecord_InsertsRowWithExpectedValues(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	tr := newTracker(pool)

	attr := Attribution{
		Branch:     "feature/x",
		PRNumber:   "42",
		CommitSHA:  "abc123",
		Team:       "platform",
		Feature:    "search",
		Repository: "acme/lens",
	}

	pool.ExpectExec(`INSERT INTO branch_spend`).
		WithArgs(
			"feature/x", "42", "abc123", "acme/lens",
			"platform", "search", "gpt-4o",
			100, 50, 0.75,
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := tr.Record(context.Background(), attr, "gpt-4o", 100, 50, 0.75); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRecord_EmptyBranchStillInserts(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	tr := newTracker(pool)

	pool.ExpectExec(`INSERT INTO branch_spend`).
		WithArgs(
			"", "", "", "", "", "", "gpt-4o-mini",
			10, 5, 0.0015,
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := tr.Record(context.Background(), Attribution{}, "gpt-4o-mini", 10, 5, 0.0015); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestGetBranchSpend_AggregatesSpend(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	tr := newTracker(pool)

	now := time.Now().UTC()
	first := now.Add(-time.Hour)

	pool.ExpectQuery(`FROM branch_spend`).
		WithArgs("feature/x", "acme/lens").
		WillReturnRows(
			pgxmock.NewRows([]string{
				"branch", "pr_number", "repository",
				"total_cost", "total_input", "total_output",
				"request_count", "first_seen", "last_seen",
			}).AddRow(
				"feature/x", "42", "acme/lens",
				float64(1.25), int64(2000), int64(800),
				int64(15), first, now,
			),
		)

	got, err := tr.GetBranchSpend(context.Background(), "feature/x", "acme/lens")
	if err != nil {
		t.Fatalf("GetBranchSpend: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil BranchSpend")
	}
	if got.Branch != "feature/x" || got.PRNumber != "42" || got.Repository != "acme/lens" {
		t.Errorf("identity mismatch: %+v", got)
	}
	if math.Abs(got.TotalCostUSD-1.25) > 1e-9 {
		t.Errorf("TotalCostUSD = %v, want 1.25", got.TotalCostUSD)
	}
	if got.TotalInputTokens != 2000 || got.TotalOutputTokens != 800 {
		t.Errorf("token totals = %d/%d, want 2000/800", got.TotalInputTokens, got.TotalOutputTokens)
	}
	if got.RequestCount != 15 {
		t.Errorf("RequestCount = %d, want 15", got.RequestCount)
	}
}

func TestGetBranchSpend_UnknownBranchReturnsNil(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	tr := newTracker(pool)

	pool.ExpectQuery(`FROM branch_spend`).
		WithArgs("does-not-exist", "acme/lens").
		WillReturnRows(pgxmock.NewRows([]string{
			"branch", "pr_number", "repository",
			"total_cost", "total_input", "total_output",
			"request_count", "first_seen", "last_seen",
		}))

	got, err := tr.GetBranchSpend(context.Background(), "does-not-exist", "acme/lens")
	if err != nil {
		t.Fatalf("expected nil error for unknown branch, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result for unknown branch, got %+v", got)
	}
}

func TestGetTopBranches_ReturnsBranchesSortedByCost(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	tr := newTracker(pool)

	now := time.Now().UTC()
	pool.ExpectQuery(`ORDER BY SUM\(cost_usd\) DESC`).
		WithArgs("acme/lens", 10).
		WillReturnRows(
			pgxmock.NewRows([]string{
				"branch", "pr_number", "repository",
				"total_cost", "total_input", "total_output",
				"request_count", "first_seen", "last_seen",
			}).
				AddRow("expensive", "10", "acme/lens", float64(100.0), int64(1), int64(1), int64(1), now, now).
				AddRow("medium", "20", "acme/lens", float64(50.0), int64(1), int64(1), int64(1), now, now).
				AddRow("cheap", "30", "acme/lens", float64(5.0), int64(1), int64(1), int64(1), now, now),
		)

	got, err := tr.GetTopBranches(context.Background(), "acme/lens", 10)
	if err != nil {
		t.Fatalf("GetTopBranches: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d branches, want 3", len(got))
	}
	if got[0].Branch != "expensive" || got[1].Branch != "medium" || got[2].Branch != "cheap" {
		t.Errorf("branch order: %v / %v / %v", got[0].Branch, got[1].Branch, got[2].Branch)
	}
	if got[0].TotalCostUSD <= got[1].TotalCostUSD || got[1].TotalCostUSD <= got[2].TotalCostUSD {
		t.Errorf("not sorted by cost desc: %v", []float64{got[0].TotalCostUSD, got[1].TotalCostUSD, got[2].TotalCostUSD})
	}
}
