// reputation.go — annotation reputation, computed from an append-only event log and
// MONEY-DECOUPLED BY CONSTRUCTION. This file is the ONLY place reputation lives; the earning
// path (annotation_mining.go SubmitAnnotation, :250 — earning = base + high-agreement bonus,
// :330-355) provably never references any symbol here. Reputation gates task access + display;
// it NEVER enters CreditTx. TestAnnotationReputation_MoneyBoundary (AST guard + earning
// invariance) pins the boundary.
//
// Score = clamp(ReputationBaseline + SUM(delta), 0, 1) folded over reputation_events (0066).
package mining

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/dbjson"
	"github.com/talyvor/lens/internal/metrics"
)

const (
	// ReputationBaseline — a NEW annotator starts NEUTRAL (not 1.0). Earn up via agreement
	// with consensus; fall below via disagreement.
	ReputationBaseline = 0.5
	reputationFloor    = 0.0
	reputationCeil     = 1.0

	// reputationK scales the per-task delta so a single MAXIMALLY-informative task moves the
	// score by at most ~0.05: max|delta| = K·(agreement−0.5)max·difficultyMax·diversityMax
	// = 0.25 · 0.5 · 0.4 · 1.0 = 0.05. (~3 maximally-bad tasks reach a 0.35 access floor;
	// ~8 good tasks build solid trust.)
	reputationK = 0.25

	// reputationMinConsensus — tasks more ambiguous than this (no clear majority) are NOT
	// scored: an agreement_outcome event with delta=0 marks them processed, so ~50/50 noise
	// never moves reputation.
	reputationMinConsensus = 0.6

	// reputationDiversityFloor — a task is fully credited only when scored against >= this many
	// DISTINCT other workspaces; a sparse / small-ring task (e.g. 2 annotators) is down-weighted.
	// This bounds SMALL rings; a ring of >= this size on healthy-looking tasks is the documented
	// collusion residual (see TestAnnotationReputation_GamingResistance). Reputation ≠ money
	// here, so the residual is not directly profitable.
	reputationDiversityFloor = 3.0

	reputationResolveBatch = 200 // expired tasks processed per resolution tick
)

// ReputationStore computes + records reputation over reputation_events. Read/append ONLY — it
// holds no ledger and never credits. The zero/nil store is inert.
type ReputationStore struct {
	pool pgxDB
}

// NewReputationStore wires the store over the main pool.
func NewReputationStore(pool pgxDB) *ReputationStore { return &ReputationStore{pool: pool} }

func clampReputation(v float64) float64 {
	if v < reputationFloor {
		return reputationFloor
	}
	if v > reputationCeil {
		return reputationCeil
	}
	return v
}

// reputationScore folds the event log into the current score, clamped. Package-level so the
// display path (GetAnnotatorStats) reads it without constructing a store.
func reputationScore(ctx context.Context, pool pgxDB, annotatorID string) (float64, error) {
	if pool == nil {
		return ReputationBaseline, nil
	}
	var sum float64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(delta), 0) FROM reputation_events WHERE annotator_id = $1`, annotatorID).Scan(&sum); err != nil {
		return 0, fmt.Errorf("reputation: score: %w", err)
	}
	return clampReputation(ReputationBaseline + sum), nil
}

// Score is the method form of reputationScore.
func (r *ReputationStore) Score(ctx context.Context, annotatorID string) (float64, error) {
	return reputationScore(ctx, r.pool, annotatorID)
}

const insertReputationEventSQL = `INSERT INTO reputation_events
    (annotator_id, kind, idem_key, delta, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (annotator_id, kind, idem_key) DO NOTHING`

// recordEvent appends ONE event (append-only; ON CONFLICT DO NOTHING). The metric is
// incremented only when a NEW row actually landed.
func (r *ReputationStore) recordEvent(ctx context.Context, annotatorID, kind, idemKey string, delta float64, reason any) error {
	if r == nil || r.pool == nil {
		return nil
	}
	reasonJSON, err := dbjson.Marshal(reason)
	if err != nil {
		return fmt.Errorf("reputation: marshal reason: %w", err)
	}
	tag, err := r.pool.Exec(ctx, insertReputationEventSQL, annotatorID, kind, idemKey, delta, reasonJSON)
	if err != nil {
		return fmt.Errorf("reputation: record event: %w", err)
	}
	if tag.RowsAffected() == 1 {
		metrics.IncAnnotationReputationEvent(kind)
	}
	return nil
}

// ResolveExpiredTasks — the resolution producer. For each EXPIRED task not yet resolved (no
// agreement_outcome event references it), append ONE agreement_outcome event per annotator,
// scored against the FINAL consensus (order-INDEPENDENT — every annotator on the task is scored
// against the full crowd, unlike the order-dependent submit-time agreement). Idempotent via
// UNIQUE(annotator_id, 'agreement_outcome', task_id). Returns the number of tasks resolved.
func (r *ReputationStore) ResolveExpiredTasks(ctx context.Context) (int, error) {
	if r == nil || r.pool == nil {
		return 0, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT t.id::text FROM annotation_tasks t
		WHERE t.expires_at < now()
		  AND NOT EXISTS (
		      SELECT 1 FROM reputation_events e
		      WHERE e.kind = 'agreement_outcome' AND e.idem_key = t.id::text)
		ORDER BY t.expires_at ASC
		LIMIT $1`, reputationResolveBatch)
	if err != nil {
		return 0, fmt.Errorf("reputation: scan expired tasks: %w", err)
	}
	var taskIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		taskIDs = append(taskIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	resolved := 0
	for _, taskID := range taskIDs {
		if err := r.resolveTask(ctx, taskID); err != nil {
			slog.Warn("reputation: resolve task failed (retries next tick)",
				slog.String("task_id", taskID), slog.String("error", err.Error()))
			continue
		}
		resolved++
	}
	return resolved, nil
}

// resolveTask scores one task's annotators against the final consensus and appends their
// outcome events (delta 0 + skipped when the task is too ambiguous / solo — still marked
// processed so it is not re-scanned).
func (r *ReputationStore) resolveTask(ctx context.Context, taskID string) error {
	rows, err := r.pool.Query(ctx, `SELECT annotator_id, decision FROM annotations WHERE task_id = $1`, taskID)
	if err != nil {
		return err
	}
	type ann struct{ id, decision string }
	var anns []ann
	counts := map[string]int{}
	for rows.Next() {
		var a ann
		if err := rows.Scan(&a.id, &a.decision); err != nil {
			rows.Close()
			return err
		}
		anns = append(anns, a)
		counts[a.decision]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	total := len(anns)
	if total == 0 {
		return nil
	}
	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}
	majorityFraction := float64(maxCount) / float64(total)
	difficulty := 1.0 - majorityFraction

	for _, a := range anns {
		others := total - 1
		delta := 0.0
		reason := map[string]any{"task_id": taskID, "majority_fraction": majorityFraction, "others": others}
		if others > 0 && majorityFraction >= reputationMinConsensus {
			matches := counts[a.decision] - 1 // exclude self
			if matches < 0 {
				matches = 0
			}
			agreement := float64(matches) / float64(others)
			diversity := float64(others) / reputationDiversityFloor
			if diversity > 1.0 {
				diversity = 1.0
			}
			delta = reputationK * (agreement - 0.5) * difficulty * diversity
			reason["agreement"] = agreement
			reason["diversity"] = diversity
		} else {
			reason["skipped"] = true // ambiguous / solo — delta 0, but mark the task processed
		}
		if err := r.recordEvent(ctx, a.id, "agreement_outcome", taskID, delta, reason); err != nil {
			return err
		}
	}
	return nil
}

// StartScheduler ticks ResolveExpiredTasks until ctx ends — mirrors FinalizeSweeper.StartScheduler.
func (r *ReputationStore) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Hour
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.ResolveExpiredTasks(ctx); err != nil {
				slog.Warn("reputation: resolution sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}
