package attribution

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgxDB is the subset of *pgxpool.Pool that Tracker needs. Tests use
// pgxmock; nil pool short-circuits DB writes for unit tests.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Tracker struct {
	pool pgxDB
}

type Attribution struct {
	Branch     string
	PRNumber   string
	CommitSHA  string
	Team       string
	Feature    string
	Repository string
}

type BranchSpend struct {
	Branch            string    `json:"branch"`
	PRNumber          string    `json:"pr_number"`
	Repository        string    `json:"repository"`
	TotalCostUSD      float64   `json:"total_cost_usd"`
	TotalInputTokens  int       `json:"total_input_tokens"`
	TotalOutputTokens int       `json:"total_output_tokens"`
	RequestCount      int       `json:"request_count"`
	FirstSeenAt       time.Time `json:"first_seen_at"`
	LastSeenAt        time.Time `json:"last_seen_at"`
}

func New(pool *pgxpool.Pool) *Tracker {
	// Guard the typed-nil interface trap: (*pgxpool.Pool)(nil) stored in
	// pgxDB compares != nil but panics on call.
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newTracker(db)
}

func newTracker(pool pgxDB) *Tracker {
	return &Tracker{pool: pool}
}

func (t *Tracker) ExtractAttribution(r *http.Request) Attribution {
	return Attribution{
		Branch:     r.Header.Get("X-Talyvor-Branch"),
		PRNumber:   r.Header.Get("X-Talyvor-PR"),
		CommitSHA:  r.Header.Get("X-Talyvor-Commit"),
		Team:       r.Header.Get("X-Talyvor-Team"),
		Feature:    r.Header.Get("X-Talyvor-Feature"),
		Repository: r.Header.Get("X-Talyvor-Repository"),
	}
}

const insertSQL = `INSERT INTO branch_spend (
  branch, pr_number, commit_sha, repository,
  team, feature, model,
  input_tokens, output_tokens, cost_usd
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

func (t *Tracker) Record(
	ctx context.Context,
	attr Attribution,
	model string,
	inputTokens, outputTokens int,
	costUSD float64,
) error {
	if t.pool == nil {
		return nil
	}
	if _, err := t.pool.Exec(ctx, insertSQL,
		attr.Branch, attr.PRNumber, attr.CommitSHA, attr.Repository,
		attr.Team, attr.Feature, model,
		inputTokens, outputTokens, costUSD,
	); err != nil {
		return fmt.Errorf("attribution: insert branch_spend: %w", err)
	}
	return nil
}

const getBranchSQL = `SELECT
  branch,
  pr_number,
  repository,
  SUM(cost_usd) as total_cost,
  SUM(input_tokens) as total_input,
  SUM(output_tokens) as total_output,
  COUNT(*) as request_count,
  MIN(created_at) as first_seen,
  MAX(created_at) as last_seen
FROM branch_spend
WHERE branch = $1 AND repository = $2
GROUP BY branch, pr_number, repository`

// GetBranchSpend returns aggregated spend for a branch. Returns (nil, nil)
// when the branch has no recorded rows — that's a 404 to the API caller,
// not an error. If a branch maps to multiple PR numbers we return the
// first aggregated row; typical usage is one (branch, repo) per PR.
func (t *Tracker) GetBranchSpend(ctx context.Context, branch, repository string) (*BranchSpend, error) {
	if t.pool == nil {
		return nil, nil
	}
	rows, err := t.pool.Query(ctx, getBranchSQL, branch, repository)
	if err != nil {
		return nil, fmt.Errorf("attribution: query branch_spend: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
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
		return nil, fmt.Errorf("attribution: scan branch_spend: %w", err)
	}
	return &bs, nil
}

const getTopSQL = `SELECT branch, pr_number, repository,
  SUM(cost_usd), SUM(input_tokens),
  SUM(output_tokens), COUNT(*),
  MIN(created_at), MAX(created_at)
FROM branch_spend
WHERE repository = $1
  AND created_at > NOW() - INTERVAL '30 days'
GROUP BY branch, pr_number, repository
ORDER BY SUM(cost_usd) DESC
LIMIT $2`

func (t *Tracker) GetTopBranches(ctx context.Context, repository string, limit int) ([]BranchSpend, error) {
	if t.pool == nil {
		return nil, nil
	}
	rows, err := t.pool.Query(ctx, getTopSQL, repository, limit)
	if err != nil {
		return nil, fmt.Errorf("attribution: query top branches: %w", err)
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
			return nil, fmt.Errorf("attribution: scan top branch row: %w", err)
		}
		out = append(out, bs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attribution: iterate top branches: %w", err)
	}
	return out, nil
}
