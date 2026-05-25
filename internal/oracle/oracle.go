// Package oracle wraps the annotation-mining surface from
// Batch 2 Item 4 with the "quality oracle" sampling + UI
// integration described in Batch 3 Phase 5.
//
// The annotation system itself (validation, agreement, stake)
// lives in internal/mining/annotation_mining.go. This package
// adds two things on top:
//   - CreateTaskFromRequest: deterministic 1% sampler the proxy
//     calls after each request to feed the annotation queue.
//   - GetOracleStats: the per-tenant + global rollup that the
//     /dashboard/oracle page renders.
package oracle

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
)

// SampleRate is the fraction of inbound requests that get
// fed into the annotation queue. 1% (0.01) is the spec value
// — high enough to keep the queue moving on a busy deployment,
// low enough that the upstream proxy doesn't pay a noticeable
// cost.
const SampleRate = 0.01

// ─── pgxDB shim ──────────────────────────────────

type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ─── types ───────────────────────────────────────

// OracleStats backs the /dashboard/oracle page.
type OracleStats struct {
	PendingTasks      int     `json:"pending_tasks"`
	CompletedTasks    int     `json:"completed_tasks"`
	ActiveOracles     int     `json:"active_oracles"`
	AvgAgreement      float64 `json:"avg_agreement"`
	TokensDistributed float64 `json:"tokens_distributed"`
}

// ─── Oracle ──────────────────────────────────────

// Oracle is the sampling + stats façade. It re-uses the
// AnnotationMiner for the heavy lifting (PII screening,
// 48h-TTL bookkeeping, agreement math) — this struct just
// adds the sampler + the rollup query.
type Oracle struct {
	annotator *mining.AnnotationMiner
	ledger    *mining.LedgerStore
	pool      pgxDB

	// sampleRate is configurable per-instance so tests can
	// crank it to 1.0 ("every request") without depending on
	// the global constant.
	sampleRate float64
}

// New builds an Oracle with the spec-mandated 1% sampler.
// `pool` may be nil — read paths will return zero-value stats,
// CreateTaskFromRequest becomes a no-op.
func New(annotator *mining.AnnotationMiner, ledger *mining.LedgerStore, pool *pgxpool.Pool) *Oracle {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newOracle(annotator, ledger, db, SampleRate)
}

// newOracle is the test-friendly constructor (takes pgxDB +
// caller-supplied sample rate).
func newOracle(annotator *mining.AnnotationMiner, ledger *mining.LedgerStore, db pgxDB, sampleRate float64) *Oracle {
	return &Oracle{
		annotator:  annotator,
		ledger:     ledger,
		pool:       db,
		sampleRate: sampleRate,
	}
}

// ─── CreateTaskFromRequest ───────────────────────

// CreateTaskFromRequest is the hook the proxy calls after each
// request. We sample 1% of requests deterministically — using
// an FNV hash of the requestID modulo 100 so the same request
// always lands on the same side of the gate (handy for
// debugging + idempotent retries).
//
// On a sampling hit we call AnnotationMiner.CreateTask, which
// runs the PII screen, anonymises, and inserts a 48h-TTL row.
// Returns nil when the request wasn't sampled — never errors
// just because of a sampling miss.
func (o *Oracle) CreateTaskFromRequest(
	ctx context.Context,
	requestID, promptHash string,
	responseA, responseB string,
	sourceWorkspace string,
) error {
	if !o.shouldSample(requestID) {
		return nil
	}
	if o.annotator == nil {
		return errors.New("oracle: annotator not wired")
	}
	_, err := o.annotator.CreateTask(ctx, sourceWorkspace, promptHash, responseA, responseB)
	return err
}

// shouldSample is the deterministic gate. Empty requestID falls
// back to a random sample (best-effort sampling for callers
// that don't track per-request IDs).
func (o *Oracle) shouldSample(requestID string) bool {
	if o.sampleRate <= 0 {
		return false
	}
	if o.sampleRate >= 1.0 {
		return true
	}
	if requestID == "" {
		// Use the current nanosecond as a poor man's PRNG —
		// good enough for "is this in the bottom 1%?".
		return uint64(time.Now().UnixNano()%100) < uint64(o.sampleRate*100)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(requestID))
	sum := h.Sum64()
	// Modulo against 10000 gives 0.01% resolution. Using the
	// full 64-bit hash keeps the distribution uniform — the
	// earlier "slice to 16 bits" version skewed at modest
	// sample rates.
	return sum%10000 < uint64(o.sampleRate*10000)
}

// ─── GetOracleStats ──────────────────────────────

// GetOracleStats reads the global oracle queue + earnings
// rollup. The /dashboard/oracle page calls this on load to
// render the dashboard counters.
func (o *Oracle) GetOracleStats(ctx context.Context) (*OracleStats, error) {
	stats := &OracleStats{}
	if o.pool == nil {
		return stats, nil
	}

	// Pending = open annotation tasks (expires_at > NOW()).
	row := o.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM annotation_tasks WHERE expires_at > NOW()`)
	if err := row.Scan(&stats.PendingTasks); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("oracle: count pending: %w", err)
	}

	// Completed = total annotations submitted.
	row = o.pool.QueryRow(ctx, `SELECT COUNT(*) FROM annotations`)
	if err := row.Scan(&stats.CompletedTasks); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("oracle: count completed: %w", err)
	}

	// Active oracles = workspaces currently staked.
	row = o.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM annotator_stakes WHERE staked > 0`)
	if err := row.Scan(&stats.ActiveOracles); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("oracle: count active: %w", err)
	}

	// Average agreement across recent annotations. Aggregates
	// over tasks that have ≥ 2 annotations so we have signal.
	row = o.pool.QueryRow(ctx, `
		WITH per_task AS (
		    SELECT task_id,
		           COUNT(*) AS total,
		           MAX(c) AS top_count
		    FROM (
		        SELECT task_id, decision, COUNT(*) AS c
		        FROM annotations
		        GROUP BY task_id, decision
		    ) inner_q
		    GROUP BY task_id
		)
		SELECT COALESCE(AVG(top_count::float / NULLIF(total, 0)), 0)
		FROM per_task WHERE total >= 2`)
	if err := row.Scan(&stats.AvgAgreement); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("oracle: avg agreement: %w", err)
	}

	// Total LENS distributed via the annotation track.
	row = o.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM lens_token_ledger
		WHERE type = $1`, mining.TypeAnnotationMine)
	if err := row.Scan(&stats.TokensDistributed); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("oracle: tokens distributed: %w", err)
	}

	return stats, nil
}
