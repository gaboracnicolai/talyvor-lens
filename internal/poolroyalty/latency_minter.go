// latency_minter.go — Proof-of-Improvement instance 3: proof-of-latency-locality (the LATENCY MINT).
//
// A leader-elected, flag-gated sweeper that pays a NODE for genuinely-fast service — cohort-relative +
// cost-weighted + quality-gated — off the per-(node,cohort,model) latency EWMA the descriptive capture
// (node_cohort_latency_stats) already produced, through the SAME held-ledger / U6 chokepoint as every other
// mint. It is the THIRD live caller of HeldBenchmarkAnchor. Unlike instances 1/2 (per-event, request_id-
// keyed), this is the FIRST EPOCH-SETTLED mint: a node is paid at most once per (node,cohort,model,epoch).
//
// THE READOUT (anti-farming), all in latencyScanSQL:
//   - BASELINE is a PER-CANDIDATE median of latency_ewma over nodes fingerprint-UNLINKED to that candidate
//     (mirrors eval_contribution_minter.go's evalDiscriminationSQL workspace-exclusion) — a workspace can't
//     inject slow linked nodes to lower its own bar. A cohort needs >= minUnlinked distinct unlinked
//     workspaces or it pays nothing.
//   - QUALITY GATE is EXACT per-(node,model): JOIN benchmark_node_scores ON (node_id, model), score >=
//     qualityThreshold AND sample_count >= qualityWarmup. Node-blind held probes ⇒ fast-and-wrong is closed
//     (a node can't tell a probe from live). An unscored (node,model) INNER-JOINs away ⇒ no pay.
//   - MARGIN is fractional clamp01((baseline − latency_ewma)/baseline) — only faster-than-baseline pays.
//   - COST-WEIGHT is cohort_cost/maxComplexityCost (cohort avg AnalyseComplexity score / 5) — fast-on-trivial
//     contributes ~0, fast-on-hard full.
//   - BATCH CAP: the per-candidate correlated exclusion is bounded to latencyMintBatchSize candidates per
//     RunOnce — an unbounded fleet never produces an unbounded query. A full batch is logged, not dropped.
//
// SETTLEMENT: epoch = floor(now/windowSeconds); request_id = SHA256Hex(node:feature:itr:complexity:model:
// epoch), UNIQUE. ON CONFLICT (request_id) DO NOTHING is the exactly-once-per-window guard. The amount is a
// FINAL-EWMA snapshot: RunOnce reads the CURRENT latency_ewma at settle time, and a later epoch re-reads the
// then-current EWMA — a boundary front-load burst can't lock in a fast rate then serve slow.
//
// NO-LOOP: reads node_cohort_latency_stats ⋈ inference_nodes ⋈ benchmark_node_scores ⋈
// workspace_card_fingerprints by RAW SQL; writes only node_latency_mints + the ledger (CreditHeldTx). It
// imports NONE of nodelatency/benchprobe/routingscore/proxy/inference (asserted by latency_noloop_test.go) —
// minting LENS can never change a future latency_ewma or benchmark score.
//
// INERT BY DEFAULT: rate 0 (the shipped default) ⇒ NewHeldBenchmarkAnchor refuses ⇒ nil anchor ⇒ RunOnce is
// a TOTAL no-op even with both flags on. Not reputation-bonded; still gets the U6 floor + 1000-LENS/24h rate
// cap via mintTypeList (TypeLatencyLocalityHeld).
package poolroyalty

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// latencyMintMaxComplexityCost is router.AnalyseComplexity Score()'s max (5 independent +1 points), the
	// denominator that normalizes a cohort's avg complexity into the [0,1] cost weight.
	latencyMintMaxComplexityCost = 5.0
	// latencyMintBatchSize bounds candidates processed per RunOnce (the per-candidate exclusion is
	// correlated; an unbounded fleet must not produce an unbounded query). A full batch is logged.
	latencyMintBatchSize = 500
	// latency mint defaults (in the reopen set — tune against real fleet density / score distributions).
	latencyMintDefaultWindowSeconds    = 86400 // 24h settlement window
	latencyMintDefaultMinUnlinked      = 3     // a cohort needs >= 3 distinct unlinked workspaces to price a baseline
	latencyMintDefaultQualityThreshold = 0.7   // benchmark_node_scores.score floor (per-(node,model))
	latencyMintDefaultQualityWarmup    = 5     // benchmark_node_scores.sample_count floor
)

// latencyScanSQL returns, per not-yet-claimed-this-epoch candidate (node,cohort,model) that clears the exact
// per-(node,model) quality gate, its latency + cohort cost + the PER-CANDIDATE unlinked-median baseline and
// the unlinked-workspace count. $1=epoch, $2=qualityThreshold, $3=qualityWarmup, $4=minUnlinked, $5=batch.
// Faster-than-baseline only. The LATERAL baseline mirrors evalDiscriminationSQL: exclude the candidate's own
// workspace AND its fingerprint-linked set; model is a 4th cohort key, orthogonal to the fingerprint join.
const latencyScanSQL = `WITH candidates AS (
    SELECT s.node_id, n.workspace_id,
           s.feature_category, s.input_token_range, s.complexity_bucket, s.model,
           s.latency_ewma, (s.cost_weight_accum / NULLIF(s.sample_count, 0)) AS cohort_cost
    FROM node_cohort_latency_stats s
    JOIN inference_nodes n ON n.id = s.node_id
    JOIN benchmark_node_scores q ON q.node_id = s.node_id AND q.model = s.model
    LEFT JOIN node_latency_mints m
           ON m.node_id = s.node_id
          AND m.feature_category = s.feature_category
          AND m.input_token_range = s.input_token_range
          AND m.complexity_bucket = s.complexity_bucket
          AND m.model = s.model
          AND m.epoch = $1
    WHERE m.request_id IS NULL
      AND q.score >= $2 AND q.sample_count >= $3
    LIMIT $5)
SELECT c.node_id, c.workspace_id,
       c.feature_category, c.input_token_range, c.complexity_bucket, c.model,
       c.latency_ewma, c.cohort_cost, base.baseline, base.n_unlinked
FROM candidates c
CROSS JOIN LATERAL (
    SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY s2.latency_ewma) AS baseline,
           count(DISTINCT n2.workspace_id) AS n_unlinked
    FROM node_cohort_latency_stats s2
    JOIN inference_nodes n2 ON n2.id = s2.node_id
    WHERE s2.feature_category = c.feature_category
      AND s2.input_token_range = c.input_token_range
      AND s2.complexity_bucket = c.complexity_bucket
      AND s2.model = c.model
      AND n2.workspace_id <> c.workspace_id
      AND n2.workspace_id NOT IN (
          SELECT b2.workspace_id FROM workspace_card_fingerprints a2
          JOIN workspace_card_fingerprints b2 ON a2.fingerprint_hash = b2.fingerprint_hash
          WHERE a2.workspace_id = c.workspace_id)
) base
WHERE base.baseline IS NOT NULL
  AND base.n_unlinked >= $4
  AND c.latency_ewma < base.baseline`

// latencyInsertClaimSQL claims one (node,cohort,model,epoch) ONCE. ON CONFLICT (request_id) DO NOTHING +
// RowsAffected is the exactly-once-per-window guard. The generic (request_id, contributor_workspace_id,
// minted_amount, status, finalize_after) columns let the generic FinalizeSweeper settle it unchanged.
const latencyInsertClaimSQL = `INSERT INTO node_latency_mints
    (request_id, contributor_workspace_id, latency_skill, minted_amount,
     node_id, feature_category, input_token_range, complexity_bucket, model, epoch,
     status, finalize_after)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'held', now() + ($11::bigint * interval '1 microsecond'))
ON CONFLICT (request_id) DO NOTHING`

// latencyMinterDB is the DB surface the sweeper needs: scan (Query) + per-candidate tx (Begin).
type latencyMinterDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// LatencyMinter is the flag-gated proof-of-latency-locality mint sweeper. A nil anchor (rate-0 default)
// makes it inert.
type LatencyMinter struct {
	db               latencyMinterDB
	ledger           ledgerCreditTx
	enabled          func() bool
	anchor           Anchor // HeldBenchmarkAnchor{rate}; nil ⇒ inert. The THIRD live held-benchmark anchor.
	holdWindow       time.Duration
	windowSeconds    int64
	minUnlinked      int
	qualityThreshold float64
	qualityWarmup    int
	batchSize        int
	now              func() time.Time
}

// NewLatencyMinter builds the sweeper. ratePerPoint is the LENS-per-latency-skill-point rate; REQUIRED —
// NewHeldBenchmarkAnchor refuses 0/neg/NaN/Inf, so the shipped default 0 leaves anchor nil and the minter a
// TOTAL no-op (inert by default). enabled is read per tick. This is the THIRD sanctioned non-test caller of
// NewHeldBenchmarkAnchor (pinned by the reachability guard).
func NewLatencyMinter(db latencyMinterDB, ledger ledgerCreditTx, ratePerPoint float64, enabled func() bool) *LatencyMinter {
	m := &LatencyMinter{
		db:               db,
		ledger:           ledger,
		enabled:          enabled,
		holdWindow:       72 * time.Hour,
		windowSeconds:    latencyMintDefaultWindowSeconds,
		minUnlinked:      latencyMintDefaultMinUnlinked,
		qualityThreshold: latencyMintDefaultQualityThreshold,
		qualityWarmup:    latencyMintDefaultQualityWarmup,
		batchSize:        latencyMintBatchSize,
		now:              time.Now,
	}
	if a, ok := NewHeldBenchmarkAnchor(ratePerPoint); ok {
		m.anchor = a // positive rate ⇒ live; otherwise nil ⇒ inert
	}
	return m
}

// SetHoldbackWindow overrides the 72h held→finalizable delay (non-positive keeps the default).
func (m *LatencyMinter) SetHoldbackWindow(d time.Duration) {
	if m != nil && d > 0 {
		m.holdWindow = d
	}
}

// latencyMintRow is one scanned candidate + its per-candidate baseline.
type latencyMintRow struct {
	nodeID, workspaceID                               string
	feature, inputTokenRange, complexityBucket, model string
	latencyEWMA, cohortCost, baseline                 float64
	nUnlinked                                         int
}

// RunOnce mints every currently mint-eligible (node,cohort,model) for the current epoch, each in its OWN
// claim-then-credit transaction. Total no-op BEFORE any DB access when inert. Per-candidate failures (e.g.
// ErrEarnNotVerified for an unverified node-workspace) are logged and skipped — the row re-mints next tick.
func (m *LatencyMinter) RunOnce(ctx context.Context) (int, error) {
	if m == nil || m.anchor == nil || m.enabled == nil || !m.enabled() || m.db == nil || m.ledger == nil {
		return 0, nil // INERT: rate-0 (nil anchor) or flags off ⇒ no DB access, no mint
	}
	epoch := m.now().Unix() / m.windowSeconds

	rows, err := m.db.Query(ctx, latencyScanSQL, epoch, m.qualityThreshold, m.qualityWarmup, m.minUnlinked, m.batchSize)
	if err != nil {
		return 0, err
	}
	cands := make([]latencyMintRow, 0, 16)
	for rows.Next() {
		var r latencyMintRow
		if err := rows.Scan(&r.nodeID, &r.workspaceID, &r.feature, &r.inputTokenRange, &r.complexityBucket,
			&r.model, &r.latencyEWMA, &r.cohortCost, &r.baseline, &r.nUnlinked); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	minted := 0
	for _, r := range cands {
		ok, err := m.mintOne(ctx, epoch, r)
		if err != nil {
			slog.Warn("poolroyalty: latency mint failed (row stays un-minted; retries next tick)",
				slog.String("node", r.nodeID), slog.String("workspace", r.workspaceID), slog.String("error", err.Error()))
			continue
		}
		if ok {
			minted++
		}
	}
	if len(cands) == m.batchSize {
		slog.Info("poolroyalty: latency mint sweep hit batch limit — more nodes mint next tick",
			slog.Int("batch", m.batchSize))
	}
	return minted, nil
}

// mintOne composes one candidate's latency_skill and, if positive, claims + credits it ONCE for this epoch.
func (m *LatencyMinter) mintOne(ctx context.Context, epoch int64, r latencyMintRow) (bool, error) {
	if r.nodeID == "" || r.workspaceID == "" || r.baseline <= 0 {
		return false, nil
	}
	// latency_skill = clamp01((baseline − L)/baseline) × clamp01(cohortCost / maxComplexityCost).
	margin := clamp01((r.baseline - r.latencyEWMA) / r.baseline)
	costFactor := clamp01(r.cohortCost / latencyMintMaxComplexityCost)
	latencySkill := margin * costFactor

	amount := m.anchor.Value(GainInput{HeldScore: latencySkill})
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return false, nil // not faster-than-baseline, trivial cohort, or rate-0 ⇒ no claim, no mint
	}

	requestID := latencyRequestID(r.nodeID, r.feature, r.inputTokenRange, r.complexityBucket, r.model, epoch)

	tx, err := m.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("poolroyalty: latency begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// (1) Claim ONCE for this (node,cohort,model,epoch). ON CONFLICT DO NOTHING + RowsAffected is the guard.
	tag, err := tx.Exec(ctx, latencyInsertClaimSQL,
		requestID, r.workspaceID, latencySkill, amount,
		r.nodeID, r.feature, r.inputTokenRange, r.complexityBucket, r.model, epoch,
		m.holdWindow.Microseconds())
	if err != nil {
		return false, fmt.Errorf("poolroyalty: latency insert claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // already claimed this epoch — exactly-once suppression, no credit
	}

	// (2) Credit the SERVING NODE's workspace held balance — reached ONLY on a fresh claim. CreditHeldTx →
	//     heldInner → verifyEarn(node-workspace) [U6 floor] + checkMintRateCap [1000-LENS/24h;
	//     eval_latency_locality_held ∈ mintTypeList]. An unverified node-workspace → ErrEarnNotVerified rolls
	//     back claim+credit; the row re-mints once verified.
	meta := map[string]interface{}{
		"node_id":       r.nodeID,
		"source":        "latency_locality",
		"anchor_kind":   m.anchor.Kind(),
		"latency_skill": latencySkill,
		"epoch":         epoch,
		"cohort":        r.feature + "/" + r.inputTokenRange + "/" + r.complexityBucket + "/" + r.model,
	}
	if err := m.ledger.CreditHeldTx(ctx, tx, r.workspaceID, amount, TypeLatencyLocalityHeld,
		"proof-of-latency-locality: cohort-relative fast service", meta); err != nil {
		return false, fmt.Errorf("poolroyalty: latency credit node-workspace (held): %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("poolroyalty: latency commit mint: %w", err)
	}
	return true, nil
}

// StartScheduler ticks RunOnce until ctx ends. Leader-elected at the call site (main wires it under
// haComps.leader.Run). Inert until the rate + both flags are on.
func (m *LatencyMinter) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Minute
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.RunOnce(ctx); err != nil {
				slog.Warn("poolroyalty: latency mint sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}

// latencyRequestID is the deterministic once-per-window idempotency key (the distill_royalty_mints composite
// pattern): SHA256Hex(node_id:feature_category:input_token_range:complexity_bucket:model:epoch).
func latencyRequestID(nodeID, feature, inputTokenRange, complexityBucket, model string, epoch int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%s:%s:%d",
		nodeID, feature, inputTokenRange, complexityBucket, model, epoch)))
	return hex.EncodeToString(h[:])
}

func clamp01(x float64) float64 {
	if math.IsNaN(x) || x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
