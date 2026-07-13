// routing_prediction_minter.go — Proof-of-Improvement instance 2: proof-of-routing-prediction.
//
// A leader-elected, flag-gated sweeper that pays a contributor whose routing PREDICTION ("cohort C →
// model M") was PROVEN skill-above-baseline on the verifier-held eval slice — ONCE per scored prediction,
// off the routing_prediction_scores the scorer already produced, through the SAME held-ledger / U6
// chokepoint as every other mint. It is the SECOND live caller of HeldBenchmarkAnchor (Proof-of-
// Improvement piece 1, PR #248): the mint amount is rate × clamp01(skill_margin).
//
// THE READOUT (anti-farming): skill_margin = clamp01(avg(score of M) − avg(score of the baseline)) on
// the cohort's held eval slice, computed UPSTREAM by internal/routingscore. That scorer EXCLUDES the
// predictor's own workspace AND its owner-linkage fingerprint-linked set from the slice (the same
// workspace_card_fingerprints self-deal guard the royalty/eval-contribution paths use), so a predictor
// can never be scored on items it planted — a self-dealt score never reaches routing_prediction_scores,
// hence never reaches this claim. The only residual farm vector (manufacturing a weak baseline via
// fabricated receipts) is the PoVI-wide gateway-bound request_id receipt-trust gate, a GO-LIVE gate.
//
// NO-LOOP: this minter only READS routing_prediction_scores ⋈ routing_predictions (by SQL) and WRITES
// only the ledger + routing_prediction_mints. It NEVER writes the scores / predictions / routing weights
// — so a mint can never feed the score it is paid on. It imports no scorer/routing/proxy/inference symbol
// (asserted by routing_prediction_noloop_test.go). Minting LENS cannot change a future skill_margin
// (the score is eval.StaticScore(M) vs the Advisor baseline — neither reads the ledger).
//
// INERT BY DEFAULT: with rate 0 (the shipped default) NewHeldBenchmarkAnchor refuses to construct → the
// anchor is nil → RunOnce is a TOTAL no-op even with both flags on. A live mint requires BOTH
// LENS_ROUTING_PREDICTION_MINTING_ENABLED and LENS_PROOF_OF_IMPROVEMENT_ENABLED on AND a positive rate.
// It is NOT reputation-bonded (symmetric with the eval-contribution mint); it still gets the U6 floor +
// the 1000-LENS/24h rate cap via mintTypeList.
package poolroyalty

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// routingMintBatchLimit bounds one tick's work; a full batch is logged, not silently dropped.
const routingMintBatchLimit = 500

// routingScanSQL finds scored predictions with no mint claim yet. The claim's request_id IS the score's
// prediction_id. It joins routing_predictions for the payee workspace (the prediction's author). Only
// already-scored predictions appear — and a score exists ONLY if the scorer cleared its MinSliceSize
// warmup with the author EXCLUDED from the slice, so there is nothing to re-check here.
var routingScanSQL = fmt.Sprintf(`SELECT s.prediction_id, p.workspace_id, s.skill_margin
FROM routing_prediction_scores s
JOIN routing_predictions p ON p.id = s.prediction_id
LEFT JOIN routing_prediction_mints m ON m.request_id = s.prediction_id
WHERE m.request_id IS NULL
LIMIT %d`, routingMintBatchLimit)

// routingInsertClaimSQL claims one scored prediction ONCE. ON CONFLICT (request_id) DO NOTHING +
// RowsAffected is the exactly-once guard (the eval_contribution_mints / pool_royalty_mints pattern). The
// generic (request_id, contributor_workspace_id, minted_amount, status, finalize_after) columns let the
// generic FinalizeSweeper settle it unchanged. request_id = the score's prediction_id.
const routingInsertClaimSQL = `INSERT INTO routing_prediction_mints
    (request_id, contributor_workspace_id, skill_margin, minted_amount, status, finalize_after)
VALUES ($1, $2, $3, $4, 'held', now() + ($5::bigint * interval '1 microsecond'))
ON CONFLICT (request_id) DO NOTHING`

// routingMinterDB is the DB surface the sweeper needs: scan (Query) + per-row tx (Begin).
type routingMinterDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// RoutingPredictionMinter is the flag-gated proof-of-routing-prediction mint sweeper. A nil anchor (the
// rate-0 default) makes it inert.
type RoutingPredictionMinter struct {
	db         routingMinterDB
	ledger     ledgerCreditTx
	enabled    func() bool
	anchor     Anchor // HeldBenchmarkAnchor{rate}; nil ⇒ inert (rate ≤ 0). The SECOND live held-benchmark anchor.
	holdWindow time.Duration
}

// NewRoutingPredictionMinter builds the sweeper. ratePerPoint is the LENS-per-skill-margin-point rate; it
// is REQUIRED — NewHeldBenchmarkAnchor refuses 0/neg/NaN/Inf, so the shipped default 0 leaves anchor nil
// and the minter a TOTAL no-op (inert by default). enabled is read per tick (both gating flags). This is
// the SECOND sanctioned non-test caller of NewHeldBenchmarkAnchor (pinned by the reachability guard).
func NewRoutingPredictionMinter(db routingMinterDB, ledger ledgerCreditTx, ratePerPoint float64, enabled func() bool) *RoutingPredictionMinter {
	m := &RoutingPredictionMinter{
		db:         db,
		ledger:     ledger,
		enabled:    enabled,
		holdWindow: 72 * time.Hour,
	}
	if a, ok := NewHeldBenchmarkAnchor(ratePerPoint); ok {
		m.anchor = a // positive rate ⇒ live; otherwise nil ⇒ inert
	}
	return m
}

// SetHoldbackWindow overrides the 72h held→finalizable delay (non-positive keeps the default).
func (m *RoutingPredictionMinter) SetHoldbackWindow(d time.Duration) {
	if m != nil && d > 0 {
		m.holdWindow = d
	}
}

// RunOnce mints every currently mint-eligible scored prediction, each in its OWN claim-then-credit
// transaction. Total no-op BEFORE any DB access when inert (nil anchor / flag off / missing deps).
// Per-row failures (e.g. ErrEarnNotVerified for an unverified author) are logged and skipped — the
// prediction stays un-minted and re-eligible next tick (the U6 floor DELAYS, never forfeits).
func (m *RoutingPredictionMinter) RunOnce(ctx context.Context) (int, error) {
	if m == nil || m.anchor == nil || m.enabled == nil || !m.enabled() || m.db == nil || m.ledger == nil {
		return 0, nil // INERT: rate-0 (nil anchor) or flags off ⇒ no DB access, no mint
	}
	rows, err := m.db.Query(ctx, routingScanSQL)
	if err != nil {
		return 0, err
	}
	type cand struct {
		predictionID, workspaceID string
		skillMargin               float64
	}
	cands := make([]cand, 0, 16)
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.predictionID, &c.workspaceID, &c.skillMargin); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	minted := 0
	for _, c := range cands {
		ok, err := m.mintOne(ctx, c.predictionID, c.workspaceID, c.skillMargin)
		if err != nil {
			slog.Warn("poolroyalty: routing-prediction mint failed (prediction stays un-minted; retries next tick)",
				slog.String("workspace", c.workspaceID), slog.String("error", err.Error()))
			continue
		}
		if ok {
			minted++
		}
	}
	if len(cands) == routingMintBatchLimit {
		slog.Info("poolroyalty: routing-prediction sweep hit batch limit — more predictions mint next tick",
			slog.Int("batch", routingMintBatchLimit))
	}
	return minted, nil
}

// mintOne values one scored prediction's skill_margin and, if positive, claims + credits it ONCE.
// Returns (true,nil) on a fresh mint; (false,nil) on a no-op (non-positive amount, already-claimed);
// (false,err) on a failure that rolls the row back for a later retry.
func (m *RoutingPredictionMinter) mintOne(ctx context.Context, predictionID, workspaceID string, skillMargin float64) (bool, error) {
	if predictionID == "" || workspaceID == "" {
		return false, nil
	}

	// amount = rate × clamp01(skill_margin). skill_margin is already clamp01'd by the scorer; the anchor
	// clamps again, so a non-positive or out-of-range value yields nothing to mint.
	valuation := m.anchor.Value(GainInput{HeldScore: skillMargin}) // Tier-2 float LENS
	if math.IsNaN(valuation) || math.IsInf(valuation, 0) || valuation <= 0 {
		return false, nil // skill_margin ≤ 0 (M did not beat the baseline) or rate-0 ⇒ no claim, no mint
	}
	amount := microFloorLENS(valuation) // SEC-2 site #4: mint valuation → µLENS, rounded DOWN
	if amount <= 0 {
		return false, nil // sub-µLENS valuation ⇒ no claim, no mint
	}

	tx, err := m.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("poolroyalty: routing begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// (1) Claim the prediction ONCE. ON CONFLICT DO NOTHING + RowsAffected is the exactly-once guard.
	tag, err := tx.Exec(ctx, routingInsertClaimSQL, predictionID, workspaceID, skillMargin, amount, m.holdWindow.Microseconds())
	if err != nil {
		return false, fmt.Errorf("poolroyalty: routing insert claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // already claimed — exactly-once suppression, no credit
	}

	// (2) Credit the PREDICTION AUTHOR's held balance — reached ONLY on a fresh claim. CreditHeldTx →
	//     heldInner → verifyEarn(author) [U6 floor] + reputation-bond code path (no-op for this non-bonded
	//     type) + checkMintRateCap(author) [the SAME 1000-LENS/24h cap; eval_routing_prediction_held ∈
	//     mintTypeList]. An unverified author → ErrEarnNotVerified rolls back claim+credit; the prediction
	//     re-mints once verified.
	meta := map[string]interface{}{
		"prediction_id": predictionID,
		"source":        "routing_prediction",
		"anchor_kind":   m.anchor.Kind(),
		"skill_margin":  skillMargin,
	}
	if err := m.ledger.CreditHeldTx(ctx, tx, workspaceID, amount, TypeRoutingPredictionHeld,
		"proof-of-routing-prediction: skill-above-baseline routing prediction", meta); err != nil {
		return false, fmt.Errorf("poolroyalty: routing credit author (held): %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("poolroyalty: routing commit mint: %w", err)
	}
	return true, nil
}

// StartScheduler ticks RunOnce until ctx ends. Leader-elected at the call site (main wires it under
// haComps.leader.Run, like the eval-contribution mint worker). Inert until the rate + both flags are on.
func (m *RoutingPredictionMinter) StartScheduler(ctx context.Context, tick time.Duration) {
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
				slog.Warn("poolroyalty: routing-prediction mint sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}
