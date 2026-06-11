package attribution

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/backpressure"
)

// ─── ExtractFromRequest ───────────────────────────

func TestExtractFromRequest_ReadsAllHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "/x", nil)
	req.Header.Set("X-Talyvor-Workspace", "ws-1")
	req.Header.Set("X-Talyvor-Feature", "code-chat")
	req.Header.Set("X-Talyvor-Issue", "ENG-42")
	req.Header.Set("X-Talyvor-Branch", "feature%2Fauth")
	req.Header.Set("X-Talyvor-PR", "PR-123")
	req.Header.Set("X-Talyvor-Commit", "abc1234")
	req.Header.Set("X-Talyvor-Author", "alice@acme.com")
	req.Header.Set("X-Talyvor-Repo", "acme/widgets")
	req.Header.Set("X-Talyvor-User", "user-7")
	req.Header.Set("X-Talyvor-Session", "sess-xyz")

	got := ExtractFromRequest(req)
	if got.WorkspaceID != "ws-1" {
		t.Errorf("workspace = %q", got.WorkspaceID)
	}
	// Feature should have the leading "code-" prefix stripped
	// so the dashboard chip stays readable.
	if got.Feature != "chat" {
		t.Errorf("feature = %q (want chat — prefix stripped)", got.Feature)
	}
	if got.IssueID != "ENG-42" {
		t.Errorf("issue = %q", got.IssueID)
	}
	// Branch must be URL-unescaped.
	if got.Git.Branch != "feature/auth" {
		t.Errorf("branch = %q (want unescaped)", got.Git.Branch)
	}
	if got.Git.PRNumber != "PR-123" || got.Git.CommitSHA != "abc1234" {
		t.Errorf("git = %+v", got.Git)
	}
	if got.Git.Author != "alice@acme.com" || got.Git.RepoName != "acme/widgets" {
		t.Errorf("author/repo = %+v", got.Git)
	}
	if got.UserID != "user-7" || got.SessionID != "sess-xyz" {
		t.Errorf("user/session = %s/%s", got.UserID, got.SessionID)
	}
	if got.Timestamp.IsZero() {
		t.Error("timestamp must be stamped")
	}
}

func TestExtractFromRequest_MissingHeadersAreEmpty(t *testing.T) {
	// A bare request — no Talyvor headers at all. ExtractFromRequest
	// must still return a usable struct (empty strings, not error).
	req := httptest.NewRequest("POST", "/x", nil)
	got := ExtractFromRequest(req)
	if got.WorkspaceID != "" || got.Git.Branch != "" || got.Git.PRNumber != "" {
		t.Fatalf("missing headers should yield empty strings: %+v", got)
	}
	if got.Timestamp.IsZero() {
		t.Error("timestamp should still be stamped")
	}
}

func TestExtractFromRequest_NumericPRStaysString(t *testing.T) {
	// Spec: "PR numbers stored as strings (handles "123" and "PR-123")".
	req := httptest.NewRequest("POST", "/x", nil)
	req.Header.Set("X-Talyvor-PR", "123")
	if got := ExtractFromRequest(req); got.Git.PRNumber != "123" {
		t.Fatalf("pr = %q", got.Git.PRNumber)
	}
}

// ─── Store.Record ─────────────────────────────────

func TestRecord_InsertsRequestAttribution(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := newStore(pool)

	pool.ExpectExec(`INSERT INTO request_attribution`).
		WithArgs(
			"ws-1", "chat", "ENG-42",
			"feature/auth", "PR-123", "abc1234", "alice@acme.com", "acme/widgets",
			"user-7", "sess-xyz",
			100, 50, 0.0123,
			"claude-sonnet-4-6", "anthropic", int64(345),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	attr := AttributionContext{
		WorkspaceID: "ws-1",
		Feature:     "chat",
		IssueID:     "ENG-42",
		Git: GitContext{
			Branch:    "feature/auth",
			PRNumber:  "PR-123",
			CommitSHA: "abc1234",
			Author:    "alice@acme.com",
			RepoName:  "acme/widgets",
		},
		UserID:    "user-7",
		SessionID: "sess-xyz",
	}
	err = s.Record(context.Background(), attr, 100, 50, 0.0123, "claude-sonnet-4-6", "anthropic", 345*time.Millisecond)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// NEUTRALITY (#157): retiring the Tracker's branch_spend double-write must leave
// the request_attribution write — the LIVE read source since #158 — untouched.
// Pins that Store.Record still fires INSERT INTO request_attribution for the
// workspace (the same proof shape as #161's counter-neutrality pin). If this
// regresses, the retirement removed the wrong write.
func TestRecord_RequestAttributionWriteIntact_Neutrality157(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := newStore(pool)

	pool.ExpectExec(`INSERT INTO request_attribution`).
		WithArgs(
			"ws-1", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := s.Record(context.Background(), AttributionContext{WorkspaceID: "ws-1"},
		10, 5, 0.001, "claude-sonnet-4-6", "anthropic", 0); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("request_attribution write must still fire after the branch_spend retirement: %v", err)
	}
}

func TestRecord_NilPoolIsNoop(t *testing.T) {
	s := NewStore(nil)
	err := s.Record(context.Background(), AttributionContext{WorkspaceID: "ws-1"}, 0, 0, 0, "", "", 0)
	if err != nil {
		t.Fatalf("nil pool should be noop: %v", err)
	}
}

func TestRecord_EmptyWorkspaceSkipsInsert(t *testing.T) {
	// Workspace ID is the only required column. Drop the row
	// silently rather than fail — the legacy tracker preserved
	// the same liberal behaviour.
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	// NOTE: no ExpectExec — the test asserts no insert fired.
	s := newStore(pool)
	if err := s.Record(context.Background(), AttributionContext{}, 0, 0, 0, "", "", 0); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL fired: %v", err)
	}
}

// ─── GetBranchStats ───────────────────────────────

func TestGetBranchStats_AggregatesTotalsAndTopLists(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newStore(pool)

	pool.ExpectQuery(`COALESCE\(SUM\(cost_usd\)`).
		WithArgs("ws-1", "feature/auth", pgxmock.AnyArg()).
		WillReturnRows(pool.NewRows([]string{"sum", "tokens", "count"}).
			AddRow(1.25, 5000, 42))

	pool.ExpectQuery(`SELECT feature, SUM\(cost_usd\)`).
		WithArgs("ws-1", "feature/auth", pgxmock.AnyArg()).
		WillReturnRows(pool.NewRows([]string{"feature", "cost", "count"}).
			AddRow("chat", 0.7, 25).
			AddRow("agent", 0.55, 17))

	pool.ExpectQuery(`SELECT author, SUM\(cost_usd\)`).
		WithArgs("ws-1", "feature/auth", pgxmock.AnyArg()).
		WillReturnRows(pool.NewRows([]string{"author", "cost", "count"}).
			AddRow("alice@acme.com", 0.9, 30))

	stats, err := s.GetBranchStats(context.Background(), "ws-1", "feature/auth", time.Time{})
	if err != nil {
		t.Fatalf("GetBranchStats: %v", err)
	}
	if stats.TotalCostUSD != 1.25 || stats.TotalTokens != 5000 || stats.RequestCount != 42 {
		t.Fatalf("totals wrong: %+v", stats)
	}
	if len(stats.TopFeatures) != 2 || stats.TopFeatures[0].Feature != "chat" {
		t.Fatalf("features wrong: %+v", stats.TopFeatures)
	}
	if len(stats.TopAuthors) != 1 || stats.TopAuthors[0].Author != "alice@acme.com" {
		t.Fatalf("authors wrong: %+v", stats.TopAuthors)
	}
}

func TestGetBranchStats_NilPoolErrors(t *testing.T) {
	s := NewStore(nil)
	if _, err := s.GetBranchStats(context.Background(), "ws-1", "b", time.Time{}); err == nil {
		t.Fatal("expected error when pool absent")
	}
}

// ─── GetCostByBranch ──────────────────────────────

func TestGetCostByBranch_OrdersByCostDescAndRespectsLimit(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newStore(pool)

	pool.ExpectQuery(`ORDER BY SUM\(cost_usd\) DESC`).
		WithArgs("ws-1", pgxmock.AnyArg(), 3).
		WillReturnRows(pool.NewRows([]string{"branch", "cost", "count"}).
			AddRow("main", 5.0, 200).
			AddRow("feature/x", 2.5, 80).
			AddRow("feature/y", 0.5, 12))

	got, err := s.GetCostByBranch(context.Background(), "ws-1", time.Time{}, 3)
	if err != nil {
		t.Fatalf("GetCostByBranch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].CostUSD < got[i].CostUSD {
			t.Errorf("not sorted DESC: %+v", got)
		}
	}
}

// ─── GetPRStats ───────────────────────────────────

func TestGetPRStats_AggregatesAndDedupes(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newStore(pool)

	pool.ExpectQuery(`SELECT\s+COALESCE\(SUM\(cost_usd\)`).
		WithArgs("ws-1", "PR-123").
		WillReturnRows(pool.NewRows([]string{"sum", "tokens", "count"}).
			AddRow(0.45, 1200, 18))

	pool.ExpectQuery(`ARRAY_REMOVE\(ARRAY_AGG`).
		WithArgs("ws-1", "PR-123").
		WillReturnRows(pool.NewRows([]string{"commits", "authors"}).
			AddRow([]string{"sha-a", "sha-b"}, []string{"alice", "bob"}))

	stats, err := s.GetPRStats(context.Background(), "ws-1", "PR-123")
	if err != nil {
		t.Fatalf("GetPRStats: %v", err)
	}
	if stats.TotalCostUSD != 0.45 || stats.RequestCount != 18 {
		t.Fatalf("totals wrong: %+v", stats)
	}
	if len(stats.Commits) != 2 || stats.Commits[0] != "sha-a" {
		t.Fatalf("commits wrong: %+v", stats.Commits)
	}
	if len(stats.Authors) != 2 || stats.Authors[0] != "alice" {
		t.Fatalf("authors wrong: %+v", stats.Authors)
	}
}

func TestGetPRStats_RequiresPRNumber(t *testing.T) {
	pool, _ := pgxmock.NewPool()
	t.Cleanup(pool.Close)
	s := newStore(pool)
	if _, err := s.GetPRStats(context.Background(), "ws-1", ""); err == nil {
		t.Fatal("expected error for empty pr_number")
	}
}

// ─── RecordAsync ──────────────────────────────────

func TestRecordAsync_NilStoreSurvives(t *testing.T) {
	// Documented contract: nil receiver is safe.
	var s *Store
	s.RecordAsync(AttributionContext{}, 0, 0, 0, "", "", 0)
}

func TestRecordAsync_NilPoolSurvives(t *testing.T) {
	s := NewStore(nil)
	s.RecordAsync(AttributionContext{WorkspaceID: "ws-1"}, 0, 0, 0, "", "", 0)
}

// ─── HTTP header sanity ───────────────────────────

func TestExtractFromRequest_StripsLowercaseFeaturePrefix(t *testing.T) {
	// Header names are case-insensitive in net/http; the prefix
	// strip is value-side and case-sensitive.
	req, _ := http.NewRequest("POST", "/x", nil)
	req.Header.Set("X-Talyvor-Feature", "code-test-gen")
	if got := ExtractFromRequest(req); got.Feature != "test-gen" {
		t.Fatalf("feature = %q", got.Feature)
	}
}

// #151: the scoped repo/branch reads must filter request_attribution by
// workspace_id ($1) — the regex pins the WHERE clause, WithArgs pins wsA as the
// bound value. Without the workspace filter a tenant would read every tenant's
// spend for a shared repo name.
func TestGetBranchSpendForWorkspace_ScopesByWorkspace(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := newStore(pool)
	now := time.Now()
	cols := []string{"branch", "pr_number", "repo_name", "cost", "in", "out", "count", "first", "last"}
	pool.ExpectQuery(`FROM request_attribution[\s\S]*workspace_id = \$1`).
		WithArgs("wsA", "feature/x", "acme/lens").
		WillReturnRows(pool.NewRows(cols).AddRow("feature/x", "42", "acme/lens", 10.0, 100, 50, 5, now, now))

	got, err := s.GetBranchSpendForWorkspace(context.Background(), "wsA", "feature/x", "acme/lens")
	if err != nil {
		t.Fatalf("GetBranchSpendForWorkspace: %v", err)
	}
	if got == nil || got.Repository != "acme/lens" || got.TotalCostUSD != 10.0 {
		t.Fatalf("got %+v, want repo acme/lens / cost 10.0", got)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("query must filter request_attribution by workspace_id (wsA as $1): %v", err)
	}
}

func TestGetTopBranchesForWorkspace_ScopesByWorkspace(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := newStore(pool)
	now := time.Now()
	cols := []string{"branch", "pr_number", "repo_name", "cost", "in", "out", "count", "first", "last"}
	pool.ExpectQuery(`FROM request_attribution[\s\S]*workspace_id = \$1`).
		WithArgs("wsA", "acme/lens", 10).
		WillReturnRows(pool.NewRows(cols).AddRow("b1", "1", "acme/lens", 9.0, 90, 45, 3, now, now))

	out, err := s.GetTopBranchesForWorkspace(context.Background(), "wsA", "acme/lens", 10)
	if err != nil {
		t.Fatalf("GetTopBranchesForWorkspace: %v", err)
	}
	if len(out) != 1 || out[0].Repository != "acme/lens" {
		t.Fatalf("got %+v, want 1 row for acme/lens", out)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("query must filter request_attribution by workspace_id (wsA as $1): %v", err)
	}
}

// ─── RecordAsync writer bound (#122) ──────────────

// countingDB is a minimal pgxDB whose Exec signals execCh, letting the
// async-path tests synchronize without sleeps-as-assertions.
type countingDB struct{ execCh chan struct{} }

func (c *countingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	c.execCh <- struct{}{}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}
func (c *countingDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (c *countingDB) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }

// A saturated limiter sheds RecordAsync — no goroutine, no insert — and
// admits again once the slot frees.
func TestRecordAsync_LimiterSaturated_Sheds(t *testing.T) {
	db := &countingDB{execCh: make(chan struct{}, 4)}
	s := newStore(db)
	l := backpressure.New(1)
	s.SetWriteLimiter(l)

	if !l.TryAcquire() { // occupy the only slot, as an in-flight writer would
		t.Fatal("setup: could not occupy the slot")
	}
	s.RecordAsync(AttributionContext{WorkspaceID: "ws-1"}, 1, 1, 0.01, "m", "p", time.Millisecond)
	select {
	case <-db.execCh:
		t.Fatal("saturated limiter must shed the record — no insert may fire")
	case <-time.After(100 * time.Millisecond):
	}
	if l.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", l.Dropped())
	}

	l.Release()
	s.RecordAsync(AttributionContext{WorkspaceID: "ws-1"}, 1, 1, 0.01, "m", "p", time.Millisecond)
	select {
	case <-db.execCh:
	case <-time.After(2 * time.Second):
		t.Fatal("freed limiter must admit the record")
	}
}

// A nil limiter (not wired / bound disabled) preserves pre-limiter behavior.
func TestRecordAsync_NilLimiter_Writes(t *testing.T) {
	db := &countingDB{execCh: make(chan struct{}, 1)}
	s := newStore(db)
	s.RecordAsync(AttributionContext{WorkspaceID: "ws-1"}, 1, 1, 0.01, "m", "p", time.Millisecond)
	select {
	case <-db.execCh:
	case <-time.After(2 * time.Second):
		t.Fatal("nil limiter must not gate RecordAsync")
	}
}
