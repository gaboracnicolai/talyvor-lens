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
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/dbjson"
	"github.com/talyvor/lens/internal/metrics"
)

const (
	// ReputationBaseline — a NEW annotator starts NEUTRAL (not 1.0). Earn up via agreement
	// with consensus; fall below via disagreement.
	ReputationBaseline = 0.5
	reputationFloor    = 0.0
	reputationCeil     = 1.0

	// AccessFloor — the minimum reputation to CLAIM a new task (the PR2 gate, GetPendingTask).
	// Set BELOW the baseline so a new annotator (baseline 0.5) and a dormant-decayed annotator
	// (decay floors AT baseline, never below) are NEVER gated — only an annotator who has
	// actively DISAGREED below the floor loses new-task access. MONEY-DECOUPLED: this gates who
	// claims a NEW task, never what a holding annotator earns (SubmitAnnotation is untouched).
	AccessFloor = 0.35

	// DormancyDays — an annotator with no annotation in this many days is "dormant"; their
	// EARNED reputation (the part above baseline) then decays at ReputationDecayRate/day back
	// toward baseline (PR3, DecayDormant). Decay FLOORS AT baseline and never below — below-
	// baseline is reserved for active disagreement, so a dormant annotator is never benched
	// (baseline 0.5 stays above AccessFloor 0.35). ReputationDecayRate (0.01) is reused from
	// annotation_mining.go — not redefined.
	DormancyDays = 7

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

// Reset is the admin re-entry path: it APPENDS an admin_reset event that lands the annotator
// back at the baseline (it is NOT an UPDATE/DELETE of the log — the immutability trigger stays
// intact and prior events remain for audit). delta = −rawSum (the negation of the current
// summed deltas) so the new sum is 0 and the score is exactly the baseline regardless of any
// clamping. by/note are recorded in the reason JSONB for audit. Returns the post-reset score.
func (r *ReputationStore) Reset(ctx context.Context, annotatorID, by, note string) (float64, error) {
	if r == nil || r.pool == nil {
		return ReputationBaseline, nil
	}
	var rawSum float64
	if err := r.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(delta), 0) FROM reputation_events WHERE annotator_id = $1`, annotatorID).Scan(&rawSum); err != nil {
		return 0, fmt.Errorf("reputation: reset read sum: %w", err)
	}
	delta := -rawSum
	idemKey := strconv.FormatInt(time.Now().UnixNano(), 10) // a distinct event per reset
	reason := map[string]any{"by": by, "note": note}
	if err := r.recordEvent(ctx, annotatorID, "admin_reset", idemKey, delta, reason); err != nil {
		return 0, err
	}
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

// RecordEvent appends ONE workspace-keyed reputation event best-effort (pool, non-tx). Exported for
// the P1 #9 reputation-bonded-minting signal emitters (challenge_pass), wired only when the flag is
// on. Append-only + idempotent like recordEvent. workspaceID lands in annotator_id (that column holds
// the workspace id — annotation_mining keys identically).
func (r *ReputationStore) RecordEvent(ctx context.Context, workspaceID, kind, idemKey string, delta float64, reason any) error {
	return r.recordEvent(ctx, workspaceID, kind, idemKey, delta, reason)
}

// RecordEventTx appends ONE workspace-keyed reputation event on the CALLER'S tx — so a slash and its
// reputation drop commit atomically (invariant 4). Exported for the P1 #9 slash emitter, wired only
// when the flag is on. Append-only + idempotent (ON CONFLICT DO NOTHING).
func (r *ReputationStore) RecordEventTx(ctx context.Context, tx pgx.Tx, workspaceID, kind, idemKey string, delta float64, reason any) error {
	reasonJSON, err := dbjson.Marshal(reason)
	if err != nil {
		return fmt.Errorf("reputation: marshal reason (tx): %w", err)
	}
	tag, err := tx.Exec(ctx, insertReputationEventSQL, workspaceID, kind, idemKey, delta, reasonJSON)
	if err != nil {
		return fmt.Errorf("reputation: record event (tx): %w", err)
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

// dormantDecayCandidatesSQL finds annotators ABOVE baseline (raw_sum > 0) whose last annotation
// is older than DormancyDays — the decay candidates — with their summed score, last activity, and
// most recent prior decay date (NULL if none). The CTEs keep raw_sum and last_activity separate so
// the annotations join never fans out the event sum.
const dormantDecayCandidatesSQL = `
WITH scores AS (
    SELECT annotator_id, SUM(delta) AS raw_sum
    FROM reputation_events GROUP BY annotator_id
),
activity AS (
    SELECT annotator_id, MAX(created_at) AS last_activity
    FROM annotations GROUP BY annotator_id
),
last_decay AS (
    SELECT annotator_id, MAX(idem_key) AS last_decay_key
    FROM reputation_events WHERE kind = 'decay' GROUP BY annotator_id
)
SELECT s.annotator_id, s.raw_sum, a.last_activity, ld.last_decay_key
FROM scores s
JOIN activity a ON a.annotator_id = s.annotator_id
LEFT JOIN last_decay ld ON ld.annotator_id = s.annotator_id
WHERE s.raw_sum > 0
  AND a.last_activity < now() - make_interval(days => $1)`

// DecayDormant — the dormancy decay sweep (PR3). For each annotator ABOVE baseline with no
// annotation in DormancyDays, append ONE 'decay' event keyed by the run date that erodes the
// earned-above-baseline reputation toward baseline at ReputationDecayRate/day:
//
//	delta = −min(ReputationDecayRate · newDormantDays, currentScore − baseline)
//
// This single clamped catch-up event (NOT one event per missed day) is:
//   - catch-up safe — newDormantDays counts every day since the last decay (or dormancy onset),
//     so an outage is made whole in one event, each missed day applied exactly once;
//   - idempotent per day — keyed by the run date via UNIQUE(annotator_id,'decay',decay_date),
//     and newDormantDays<=0 once today is already decayed, so a re-run is a no-op;
//   - floored AT baseline — the headroom clamp (currentScore − baseline) means decay can never
//     cross baseline. An at/below-baseline annotator is excluded by raw_sum > 0 → no event.
//
// MONEY-DECOUPLED: it appends a reputation event; it never touches the ledger. Returns the count
// of annotators decayed this sweep.
func (r *ReputationStore) DecayDormant(ctx context.Context) (int, error) {
	if r == nil || r.pool == nil {
		return 0, nil
	}
	rows, err := r.pool.Query(ctx, dormantDecayCandidatesSQL, DormancyDays)
	if err != nil {
		return 0, fmt.Errorf("reputation: decay candidates: %w", err)
	}
	type cand struct {
		id           string
		rawSum       float64
		lastActivity time.Time
		lastDecayKey *string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.rawSum, &c.lastActivity, &c.lastDecayKey); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	const dateLayout = "2006-01-02"
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	decayed := 0
	for _, c := range cands {
		// Dormancy begins DormancyDays after the last activity; decay accrues per day from there
		// (last-activity + 7d == 0 dormant days; each further day == one day of decay). A prior
		// decay moves the accrual start forward so each missed day is counted exactly once.
		la := c.lastActivity.UTC()
		dormancyStart := time.Date(la.Year(), la.Month(), la.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, DormancyDays)
		effectiveLast := dormancyStart
		if c.lastDecayKey != nil {
			if d, e := time.Parse(dateLayout, *c.lastDecayKey); e == nil && d.After(effectiveLast) {
				effectiveLast = d
			}
		}
		newDormantDays := int(today.Sub(effectiveLast).Hours() / 24)
		if newDormantDays <= 0 {
			continue // not yet dormant, or already decayed today (idempotent)
		}
		currentScore := clampReputation(ReputationBaseline + c.rawSum)
		headroom := currentScore - ReputationBaseline
		if headroom <= 1e-9 {
			continue // at/below baseline — nothing earned to erode
		}
		decayMag := ReputationDecayRate * float64(newDormantDays)
		if decayMag > headroom {
			decayMag = headroom // FLOOR AT BASELINE: never erode past the earned-above-baseline amount
		}
		if decayMag <= 1e-9 {
			continue
		}
		reason := map[string]any{"dormant_days": newDormantDays, "last_activity": la.Format(time.RFC3339)}
		if err := r.recordEvent(ctx, c.id, "decay", today.Format(dateLayout), -decayMag, reason); err != nil {
			return decayed, err
		}
		decayed++
	}
	return decayed, nil
}

// StartScheduler ticks the reputation sweep until ctx ends — ONE job doing resolution + dormancy
// decay per tick (mirrors FinalizeSweeper.StartScheduler).
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
			if _, err := r.DecayDormant(ctx); err != nil {
				slog.Warn("reputation: decay sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}
