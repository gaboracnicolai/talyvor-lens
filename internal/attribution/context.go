// context.go is the richer, workspace-aware attribution surface
// added in Upgrade Batch 1 / Item 3. It coexists with the
// original Tracker (tracker.go) — Tracker keeps the legacy
// branch_spend rollup contract working, while Store (here)
// writes to request_attribution and serves the new Git-focused
// dashboard endpoints.
//
// Design choices:
//
//   - The proxy hot path calls RecordAsync, which spawns a goroutine
//     with its own short context. The proxy NEVER blocks on the DB.
//   - Missing X-Talyvor-* headers map to empty strings rather than
//     errors. Git context is additive; a request without any of
//     it still records cleanly (workspace_id + cost + model).
//   - Branch names get URL-unescaped before storage so
//     `feature%2Fauth` lands as `feature/auth` in the index.

package attribution

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GitContext is the on-the-wire git-context bundle. All fields
// optional — an empty value means "not supplied".
type GitContext struct {
	Branch    string `json:"branch"`
	PRNumber  string `json:"pr_number"`
	CommitSHA string `json:"commit_sha"`
	Author    string `json:"author"`
	RepoName  string `json:"repo_name"`
}

// AttributionContext is everything we know about a single AI
// call before tokens get counted. Populated by
// ExtractFromRequest and handed to Store.Record.
type AttributionContext struct {
	WorkspaceID string     `json:"workspace_id"`
	Feature     string     `json:"feature"`
	IssueID     string     `json:"issue_id"`
	Git         GitContext `json:"git"`
	UserID      string     `json:"user_id"`
	SessionID   string     `json:"session_id"`
	Timestamp   time.Time  `json:"timestamp"`
}

// Header length limits. Values beyond these are silently truncated before
// storage to prevent oversized strings from bloating the request_attribution
// table and to limit the blast radius of a client sending garbage headers.
// Limits are generous enough that no legitimate tool-generated value is cut.
const (
	maxIDLen     = 128  // workspace IDs, issue IDs, user IDs, session IDs
	maxNameLen   = 256  // branch names, repo names, author names, feature names
	maxSHALen    = 64   // git commit SHA (40 hex for SHA-1, 64 for SHA-256)
	maxPRNumLen  = 16   // PR number strings ("12345")
)

// truncate returns s trimmed to at most maxLen runes. Operates on runes so
// multi-byte UTF-8 characters are not split.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

// ExtractFromRequest reads the standard X-Talyvor-* headers into an
// AttributionContext. Values are truncated to their per-field limits before
// storage to guard against oversized inputs.
func ExtractFromRequest(r *http.Request) AttributionContext {
	branch := r.Header.Get("X-Talyvor-Branch")
	if decoded, err := url.QueryUnescape(branch); err == nil {
		branch = decoded
	}
	feature := r.Header.Get("X-Talyvor-Feature")
	// Strip the optional "code-" prefix Talyvor Code adds —
	// keeps the dashboard chips readable.
	feature = strings.TrimPrefix(feature, "code-")
	return AttributionContext{
		WorkspaceID: truncate(r.Header.Get("X-Talyvor-Workspace"), maxIDLen),
		Feature:     truncate(feature, maxNameLen),
		IssueID:     truncate(r.Header.Get("X-Talyvor-Issue"), maxIDLen),
		Git: GitContext{
			Branch:    truncate(branch, maxNameLen),
			PRNumber:  truncate(r.Header.Get("X-Talyvor-PR"), maxPRNumLen),
			CommitSHA: truncate(r.Header.Get("X-Talyvor-Commit"), maxSHALen),
			Author:    truncate(r.Header.Get("X-Talyvor-Author"), maxNameLen),
			RepoName:  truncate(r.Header.Get("X-Talyvor-Repo"), maxNameLen),
		},
		UserID:    truncate(r.Header.Get("X-Talyvor-User"), maxIDLen),
		SessionID: truncate(r.Header.Get("X-Talyvor-Session"), maxIDLen),
		Timestamp: time.Now().UTC(),
	}
}

// ─── Store ────────────────────────────────────────

// Store reads and writes the request_attribution table.
// nil-pool short-circuits to a no-op so unit tests can build a
// Store without standing up Postgres.
type Store struct {
	pool pgxDB
}

func NewStore(pool *pgxpool.Pool) *Store {
	// Defend against the typed-nil interface trap.
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db)
}

func newStore(pool pgxDB) *Store {
	return &Store{pool: pool}
}

// ─── Record ───────────────────────────────────────

const recordSQL = `
INSERT INTO request_attribution (
    workspace_id, feature, issue_id,
    branch, pr_number, commit_sha, author, repo_name,
    user_id, session_id,
    input_tokens, output_tokens, cost_usd,
    model, provider, latency_ms
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7, $8,
    $9, $10,
    $11, $12, $13,
    $14, $15, $16
)`

// Record inserts one observation synchronously. The proxy uses
// RecordAsync for the hot path; this entry point exists for
// callers that need a hard success/failure signal (tests,
// admin tools).
func (s *Store) Record(
	ctx context.Context,
	attr AttributionContext,
	inputTokens, outputTokens int,
	costUSD float64,
	model, provider string,
	latency time.Duration,
) error {
	if s.pool == nil {
		return nil
	}
	if strings.TrimSpace(attr.WorkspaceID) == "" {
		// workspace_id is the only required column. Drop the row
		// rather than fail the request — the legacy tracker still
		// captured what it could from the same headers.
		return nil
	}
	if _, err := s.pool.Exec(ctx, recordSQL,
		attr.WorkspaceID, attr.Feature, attr.IssueID,
		attr.Git.Branch, attr.Git.PRNumber, attr.Git.CommitSHA,
		attr.Git.Author, attr.Git.RepoName,
		attr.UserID, attr.SessionID,
		inputTokens, outputTokens, costUSD,
		model, provider, latency.Milliseconds(),
	); err != nil {
		return fmt.Errorf("attribution: insert request_attribution: %w", err)
	}
	return nil
}

// RecordAsync queues an insert on a background goroutine. The
// supplied attribution + token counts get copied by value so
// the caller can't accidentally mutate them mid-flight. Errors
// land in the structured logger; callers don't need to handle
// them. Pass context.Background() — the goroutine creates its
// own short-lived timeout context so a slow Postgres can't
// pile up goroutines.
func (s *Store) RecordAsync(
	attr AttributionContext,
	inputTokens, outputTokens int,
	costUSD float64,
	model, provider string,
	latency time.Duration,
) {
	if s == nil || s.pool == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Record(ctx, attr, inputTokens, outputTokens, costUSD, model, provider, latency); err != nil {
			slog.Warn("attribution: async record failed",
				slog.String("workspace", attr.WorkspaceID),
				slog.String("err", err.Error()))
		}
	}()
}

// ─── read APIs ────────────────────────────────────

// FeatureCost is one row in BranchStats.TopFeatures.
type FeatureCost struct {
	Feature  string  `json:"feature"`
	CostUSD  float64 `json:"cost_usd"`
	Requests int     `json:"requests"`
}

// AuthorCost is one row in BranchStats.TopAuthors.
type AuthorCost struct {
	Author   string  `json:"author"`
	CostUSD  float64 `json:"cost_usd"`
	Requests int     `json:"requests"`
}

// BranchStats is the per-branch rollup served by
// GET /v1/workspaces/:wsID/attribution/branches/:branch.
type BranchStats struct {
	Branch       string        `json:"branch"`
	TotalCostUSD float64       `json:"total_cost_usd"`
	TotalTokens  int           `json:"total_tokens"`
	RequestCount int           `json:"request_count"`
	TopFeatures  []FeatureCost `json:"top_features"`
	TopAuthors   []AuthorCost  `json:"top_authors"`
}

// PRStats is the per-PR rollup served by
// GET /v1/workspaces/:wsID/attribution/prs/:prNumber.
type PRStats struct {
	PRNumber     string   `json:"pr_number"`
	TotalCostUSD float64  `json:"total_cost_usd"`
	TotalTokens  int      `json:"total_tokens"`
	RequestCount int      `json:"request_count"`
	Commits      []string `json:"commits"`
	Authors      []string `json:"authors"`
}

// BranchCost is one row in the GET /attribution/branches list.
type BranchCost struct {
	Branch   string  `json:"branch"`
	CostUSD  float64 `json:"cost_usd"`
	Requests int     `json:"requests"`
}

const branchTotalsSQL = `
SELECT
    COALESCE(SUM(cost_usd), 0),
    COALESCE(SUM(input_tokens + output_tokens), 0),
    COUNT(*)
FROM request_attribution
WHERE workspace_id = $1 AND branch = $2 AND created_at >= $3`

const branchTopFeaturesSQL = `
SELECT feature, SUM(cost_usd), COUNT(*)
FROM request_attribution
WHERE workspace_id = $1 AND branch = $2 AND created_at >= $3
GROUP BY feature
ORDER BY SUM(cost_usd) DESC
LIMIT 10`

const branchTopAuthorsSQL = `
SELECT author, SUM(cost_usd), COUNT(*)
FROM request_attribution
WHERE workspace_id = $1 AND branch = $2 AND created_at >= $3 AND author != ''
GROUP BY author
ORDER BY SUM(cost_usd) DESC
LIMIT 10`

// GetBranchStats returns total cost / tokens / request count
// for a branch plus the top-10 features and authors driving
// the spend. `since` filters the rolling window — pass
// time.Time{} for "all time" (handled as a far-past timestamp).
func (s *Store) GetBranchStats(ctx context.Context, workspaceID, branch string, since time.Time) (*BranchStats, error) {
	if s.pool == nil {
		return nil, errors.New("attribution: store has no database configured")
	}
	if workspaceID == "" {
		return nil, errors.New("attribution: workspace_id required")
	}
	since = normaliseSince(since)

	stats := &BranchStats{Branch: branch}
	row := s.pool.QueryRow(ctx, branchTotalsSQL, workspaceID, branch, since)
	if err := row.Scan(&stats.TotalCostUSD, &stats.TotalTokens, &stats.RequestCount); err != nil {
		return nil, fmt.Errorf("attribution: branch totals: %w", err)
	}

	features, err := s.queryFeatureCosts(ctx, branchTopFeaturesSQL, workspaceID, branch, since)
	if err != nil {
		return nil, err
	}
	stats.TopFeatures = features

	authors, err := s.queryAuthorCosts(ctx, branchTopAuthorsSQL, workspaceID, branch, since)
	if err != nil {
		return nil, err
	}
	stats.TopAuthors = authors
	return stats, nil
}

const prRollupSQL = `
SELECT
    COALESCE(SUM(cost_usd), 0),
    COALESCE(SUM(input_tokens + output_tokens), 0),
    COUNT(*)
FROM request_attribution
WHERE workspace_id = $1 AND pr_number = $2`

const prDistinctSQL = `
SELECT
    ARRAY_REMOVE(ARRAY_AGG(DISTINCT commit_sha), ''),
    ARRAY_REMOVE(ARRAY_AGG(DISTINCT author), '')
FROM request_attribution
WHERE workspace_id = $1 AND pr_number = $2`

// GetPRStats aggregates every row tagged with the supplied PR
// number. Commit SHAs and authors are de-duplicated with empty
// strings stripped (a PR with one author shouldn't surface a
// `""` entry for the requests that lacked the header).
func (s *Store) GetPRStats(ctx context.Context, workspaceID, prNumber string) (*PRStats, error) {
	if s.pool == nil {
		return nil, errors.New("attribution: store has no database configured")
	}
	if workspaceID == "" || prNumber == "" {
		return nil, errors.New("attribution: workspace_id and pr_number required")
	}
	stats := &PRStats{PRNumber: prNumber}
	row := s.pool.QueryRow(ctx, prRollupSQL, workspaceID, prNumber)
	if err := row.Scan(&stats.TotalCostUSD, &stats.TotalTokens, &stats.RequestCount); err != nil {
		return nil, fmt.Errorf("attribution: pr rollup: %w", err)
	}
	row = s.pool.QueryRow(ctx, prDistinctSQL, workspaceID, prNumber)
	if err := row.Scan(&stats.Commits, &stats.Authors); err != nil {
		return nil, fmt.Errorf("attribution: pr distincts: %w", err)
	}
	sort.Strings(stats.Commits)
	sort.Strings(stats.Authors)
	return stats, nil
}

const costByBranchSQL = `
SELECT branch, SUM(cost_usd), COUNT(*)
FROM request_attribution
WHERE workspace_id = $1 AND created_at >= $2 AND branch != ''
GROUP BY branch
ORDER BY SUM(cost_usd) DESC
LIMIT $3`

// GetCostByBranch returns the top branches by total spend.
// limit ≤ 0 → 20. since zero-value → "all time".
func (s *Store) GetCostByBranch(ctx context.Context, workspaceID string, since time.Time, limit int) ([]BranchCost, error) {
	if s.pool == nil {
		return nil, errors.New("attribution: store has no database configured")
	}
	if workspaceID == "" {
		return nil, errors.New("attribution: workspace_id required")
	}
	if limit <= 0 {
		limit = 20
	}
	since = normaliseSince(since)
	rows, err := s.pool.Query(ctx, costByBranchSQL, workspaceID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("attribution: cost by branch: %w", err)
	}
	defer rows.Close()
	var out []BranchCost
	for rows.Next() {
		var bc BranchCost
		if err := rows.Scan(&bc.Branch, &bc.CostUSD, &bc.Requests); err != nil {
			return nil, fmt.Errorf("attribution: scan branch row: %w", err)
		}
		out = append(out, bc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ─── #151: workspace-scoped repo/branch reads ───────────────────────────────
// These back GET /v1/attribution/branch and /top after the #151 re-point. They
// return the SAME BranchSpend shape the legacy Tracker queries did, but source
// it from request_attribution (which carries workspace_id) filtered to the
// caller's workspace — so a tenant sees only its OWN slice of a (possibly
// shared) repository name, instead of every tenant's spend for that repo.

const branchSpendForWorkspaceSQL = `SELECT
  branch, pr_number, repo_name,
  SUM(cost_usd), SUM(input_tokens), SUM(output_tokens),
  COUNT(*), MIN(created_at), MAX(created_at)
FROM request_attribution
WHERE workspace_id = $1 AND branch = $2 AND repo_name = $3
GROUP BY branch, pr_number, repo_name`

// GetBranchSpendForWorkspace returns aggregated spend for (workspace, branch,
// repo). (nil, nil) when the workspace has no rows for it — a 404 at the API,
// not an error. Mirrors the legacy GetBranchSpend's "first aggregated row when a
// branch maps to multiple PRs" behavior.
func (s *Store) GetBranchSpendForWorkspace(ctx context.Context, workspaceID, branch, repository string) (*BranchSpend, error) {
	if s.pool == nil {
		return nil, nil
	}
	if workspaceID == "" {
		return nil, errors.New("attribution: workspace_id required")
	}
	rows, err := s.pool.Query(ctx, branchSpendForWorkspaceSQL, workspaceID, branch, repository)
	if err != nil {
		return nil, fmt.Errorf("attribution: query branch spend (scoped): %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var bs BranchSpend
	if err := rows.Scan(
		&bs.Branch, &bs.PRNumber, &bs.Repository,
		&bs.TotalCostUSD, &bs.TotalInputTokens, &bs.TotalOutputTokens,
		&bs.RequestCount, &bs.FirstSeenAt, &bs.LastSeenAt,
	); err != nil {
		return nil, fmt.Errorf("attribution: scan branch spend (scoped): %w", err)
	}
	return &bs, nil
}

const topBranchesForWorkspaceSQL = `SELECT
  branch, pr_number, repo_name,
  SUM(cost_usd), SUM(input_tokens), SUM(output_tokens),
  COUNT(*), MIN(created_at), MAX(created_at)
FROM request_attribution
WHERE workspace_id = $1 AND repo_name = $2
  AND created_at > NOW() - INTERVAL '30 days'
GROUP BY branch, pr_number, repo_name
ORDER BY SUM(cost_usd) DESC
LIMIT $3`

// GetTopBranchesForWorkspace returns the workspace's top branches for a repo by
// spend over the last 30 days (the legacy GetTopBranches window).
func (s *Store) GetTopBranchesForWorkspace(ctx context.Context, workspaceID, repository string, limit int) ([]BranchSpend, error) {
	if s.pool == nil {
		return nil, nil
	}
	if workspaceID == "" {
		return nil, errors.New("attribution: workspace_id required")
	}
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx, topBranchesForWorkspaceSQL, workspaceID, repository, limit)
	if err != nil {
		return nil, fmt.Errorf("attribution: query top branches (scoped): %w", err)
	}
	defer rows.Close()
	var out []BranchSpend
	for rows.Next() {
		var bs BranchSpend
		if err := rows.Scan(
			&bs.Branch, &bs.PRNumber, &bs.Repository,
			&bs.TotalCostUSD, &bs.TotalInputTokens, &bs.TotalOutputTokens,
			&bs.RequestCount, &bs.FirstSeenAt, &bs.LastSeenAt,
		); err != nil {
			return nil, fmt.Errorf("attribution: scan top branch (scoped): %w", err)
		}
		out = append(out, bs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attribution: iterate top branches (scoped): %w", err)
	}
	return out, nil
}

// Summary is the cross-dimension rollup returned by
// GET /v1/workspaces/:wsID/attribution/summary?days=N.
type Summary struct {
	WorkspaceID  string        `json:"workspace_id"`
	WindowDays   int           `json:"window_days"`
	TotalCostUSD float64       `json:"total_cost"`
	ByBranch     []BranchCost  `json:"by_branch"`
	ByPR         []BranchCost  `json:"by_pr"`
	ByAuthor     []AuthorCost  `json:"by_author"`
	ByRepo       []BranchCost  `json:"by_repo"`
}

const summaryTotalSQL = `
SELECT COALESCE(SUM(cost_usd), 0)
FROM request_attribution
WHERE workspace_id = $1 AND created_at >= $2`

const summaryByPRSQL = `
SELECT pr_number, SUM(cost_usd), COUNT(*)
FROM request_attribution
WHERE workspace_id = $1 AND created_at >= $2 AND pr_number != ''
GROUP BY pr_number
ORDER BY SUM(cost_usd) DESC
LIMIT 20`

const summaryByRepoSQL = `
SELECT repo_name, SUM(cost_usd), COUNT(*)
FROM request_attribution
WHERE workspace_id = $1 AND created_at >= $2 AND repo_name != ''
GROUP BY repo_name
ORDER BY SUM(cost_usd) DESC
LIMIT 20`

const summaryByAuthorSQL = `
SELECT author, SUM(cost_usd), COUNT(*)
FROM request_attribution
WHERE workspace_id = $1 AND created_at >= $2 AND author != ''
GROUP BY author
ORDER BY SUM(cost_usd) DESC
LIMIT 20`

// GetSummary builds the cross-dimension rollup for the
// dashboard's Git Attribution tab. days <= 0 → 30.
func (s *Store) GetSummary(ctx context.Context, workspaceID string, days int) (*Summary, error) {
	if s.pool == nil {
		return nil, errors.New("attribution: store has no database configured")
	}
	if workspaceID == "" {
		return nil, errors.New("attribution: workspace_id required")
	}
	if days <= 0 {
		days = 30
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	sum := &Summary{WorkspaceID: workspaceID, WindowDays: days}
	if err := s.pool.QueryRow(ctx, summaryTotalSQL, workspaceID, since).Scan(&sum.TotalCostUSD); err != nil {
		return nil, fmt.Errorf("attribution: summary total: %w", err)
	}
	branches, err := s.GetCostByBranch(ctx, workspaceID, since, 20)
	if err != nil {
		return nil, err
	}
	sum.ByBranch = branches

	prs, err := s.queryBranchCosts(ctx, summaryByPRSQL, workspaceID, since)
	if err != nil {
		return nil, err
	}
	sum.ByPR = prs

	repos, err := s.queryBranchCosts(ctx, summaryByRepoSQL, workspaceID, since)
	if err != nil {
		return nil, err
	}
	sum.ByRepo = repos

	authors, err := s.queryAuthorCosts(ctx, summaryByAuthorSQL, workspaceID, "", since)
	if err != nil {
		return nil, err
	}
	sum.ByAuthor = authors
	return sum, nil
}

// ─── small query helpers ──────────────────────────

func (s *Store) queryFeatureCosts(ctx context.Context, sql string, args ...any) ([]FeatureCost, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("attribution: query features: %w", err)
	}
	defer rows.Close()
	var out []FeatureCost
	for rows.Next() {
		var c FeatureCost
		if err := rows.Scan(&c.Feature, &c.CostUSD, &c.Requests); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) queryAuthorCosts(ctx context.Context, sql string, args ...any) ([]AuthorCost, error) {
	// queryAuthorCosts is shared between branch-scoped queries
	// (which take branch as $2) and summary-scoped queries
	// (which don't). Callers shape args accordingly.
	rows, err := s.pool.Query(ctx, sql, filterPlaceholders(args)...)
	if err != nil {
		return nil, fmt.Errorf("attribution: query authors: %w", err)
	}
	defer rows.Close()
	var out []AuthorCost
	for rows.Next() {
		var c AuthorCost
		if err := rows.Scan(&c.Author, &c.CostUSD, &c.Requests); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) queryBranchCosts(ctx context.Context, sql string, args ...any) ([]BranchCost, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("attribution: query branches: %w", err)
	}
	defer rows.Close()
	var out []BranchCost
	for rows.Next() {
		var c BranchCost
		if err := rows.Scan(&c.Branch, &c.CostUSD, &c.Requests); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// filterPlaceholders strips an empty string sitting between
// real args — used so the same query helper can be called with
// (ws, since) or (ws, branch, since) without re-wiring the SQL.
func filterPlaceholders(args []any) []any {
	out := make([]any, 0, len(args))
	for _, a := range args {
		if s, ok := a.(string); ok && s == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// normaliseSince turns a zero time.Time into a far-past
// timestamp so queries treat "since zero" as "all time".
func normaliseSince(t time.Time) time.Time {
	if t.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return t
}
