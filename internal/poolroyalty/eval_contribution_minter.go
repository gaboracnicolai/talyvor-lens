// eval_contribution_minter.go — Proof-of-Improvement instance 1: proof-of-eval-contribution.
//
// A leader-elected, flag-gated sweeper that pays a contributor for adding a DISCRIMINATING eval item
// to the verifier pool — ONCE per item, off the existing benchmark_probes data, through the SAME
// held-ledger / U6 chokepoint as every other mint. It is the FIRST live caller of HeldBenchmarkAnchor
// (Proof-of-Improvement piece 1, PR #248): the mint amount is rate × clamp01(discrimination), where
// discrimination is a HARD, externally-observed readout the author cannot inflate.
//
// THE READOUT (anti-farming): discrimination_i = clamp01(4 × Var(score_w : w ∈ G)) over G = the set of
// DISTINCT, UNLINKED grader workspaces for item i — one mean score per workspace, EXCLUDING the
// author's workspace AND the author's owner-linkage fingerprint-linked set (the same workspace_card_
// fingerprints join the royalty self-deal guard uses). Var of a [0,1] variable ≤ 0.25, so 4·Var ∈
// [0,1]: an item everyone passes / everyone fails (non-discriminating or broken) → ~0; an item that
// cleanly separates good nodes from bad → ~1. A warmup floor |G| ≥ MinUnlinkedGraders gates payment so
// one sockpuppet barely moves a variance taken over ≥N distinct unlinked workspaces.
//
// SOCKPUPPET RESIDUAL (blessed bound, not eliminated): the fingerprint link is default-ALLOW on
// missing, so a contributor who funds a second workspace with a DIFFERENT card / no card evades the
// linked-set exclusion and can grade their own item. This is bounded — not closed — by the
// MinUnlinkedGraders warmup + the U6 1000-LENS/24h author cap, and is a LOGGED pre-public-mint gate
// (the same class as the royalty cross-card wash). See the metric site below + DrawItem.
//
// NO-LOOP: this minter only READS benchmark_probes (the score substrate, whose producer benchprobe is
// import-guarded mint-free) and WRITES only the ledger + eval_contribution_mints. It never writes
// benchmark_probes / benchmark_node_scores — so a mint can never feed the score it is paid on
// (asserted by TestEvalContributionMinter_NoScoreWrites_Guard). It does NOT import benchprobe.
//
// INERT BY DEFAULT: with rate 0 (the shipped default) NewHeldBenchmarkAnchor refuses to construct →
// the anchor is nil → RunOnce is a TOTAL no-op even with both flags on. A live mint requires BOTH
// LENS_EVAL_CONTRIBUTION_MINTING_ENABLED and LENS_PROOF_OF_IMPROVEMENT_ENABLED on AND a positive rate.
package poolroyalty

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// evalSweepBatchLimit bounds one tick's work; a full batch is logged, not silently dropped.
const evalSweepBatchLimit = 500

// DefaultMinUnlinkedGraders is the warmup floor: an item pays nothing until ≥ this many DISTINCT
// unlinked grader workspaces have scored it (mirrors the routing cohort MinWorkspaces=3).
const DefaultMinUnlinkedGraders = 3

// evalScanSQL finds active, AUTHORED items with no mint claim yet (request_id == item id). Operator-
// seeded items (author_workspace_id NULL) and pending/quarantined items are excluded — only validated,
// contributed items are mint-eligible.
var evalScanSQL = fmt.Sprintf(`SELECT e.id, e.author_workspace_id
FROM benchmark_eval_items e
LEFT JOIN eval_contribution_mints m ON m.request_id = e.id
WHERE e.status = 'active' AND e.author_workspace_id IS NOT NULL AND m.request_id IS NULL
LIMIT %d`, evalSweepBatchLimit)

// evalDiscriminationSQL computes (distinct unlinked graders, population variance of their mean scores)
// for one item, EXCLUDING the author's workspace and the author's fingerprint-linked set. Read-only
// over benchmark_probes ⋈ inference_nodes (grader workspace) ⋈ workspace_card_fingerprints (linkage).
// $1 = item id, $2 = author workspace id.
const evalDiscriminationSQL = `WITH grader_scores AS (
    SELECT n.workspace_id AS grader_ws, AVG(p.score) AS ws_score
    FROM benchmark_probes p
    JOIN inference_nodes n ON n.id = p.node_id
    WHERE p.item_id = $1
      AND n.workspace_id <> $2
      AND n.workspace_id NOT IN (
          SELECT b.workspace_id FROM workspace_card_fingerprints a
          JOIN workspace_card_fingerprints b ON a.fingerprint_hash = b.fingerprint_hash
          WHERE a.workspace_id = $2)
    GROUP BY n.workspace_id)
SELECT COUNT(*)::int, COALESCE(VAR_POP(ws_score), 0) FROM grader_scores`

// evalInsertClaimSQL claims one item ONCE. ON CONFLICT (request_id) DO NOTHING + RowsAffected is the
// exactly-once guard (the pool_royalty_mints / distill_royalty_mints pattern). status='held';
// finalize_after = now + holdback. request_id = the item id.
const evalInsertClaimSQL = `INSERT INTO eval_contribution_mints
    (request_id, contributor_workspace_id, discrimination, distinct_graders, minted_amount, status, finalize_after)
VALUES ($1, $2, $3, $4, $5, 'held', now() + ($6::bigint * interval '1 microsecond'))
ON CONFLICT (request_id) DO NOTHING`

// evalMinterDB is the DB surface the sweeper needs: scan (Query) + per-item tx (Begin).
type evalMinterDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// EvalContributionMinter is the flag-gated proof-of-eval-contribution mint sweeper. A nil anchor (the
// rate-0 default) makes it inert.
type EvalContributionMinter struct {
	db         evalMinterDB
	ledger     ledgerCreditTx
	enabled    func() bool
	anchor     Anchor // HeldBenchmarkAnchor{rate}; nil ⇒ inert (rate ≤ 0). The ONE live held-benchmark anchor.
	minGraders int
	holdWindow time.Duration
}

// NewEvalContributionMinter builds the sweeper. ratePerPoint is the LENS-per-discrimination-point rate;
// it is REQUIRED — NewHeldBenchmarkAnchor refuses 0/neg/NaN/Inf, so the shipped default 0 leaves anchor
// nil and the minter a TOTAL no-op (inert by default). enabled is read per tick (both gating flags).
// This is the SOLE non-test caller of NewHeldBenchmarkAnchor (pinned by the inverted reachability guard).
func NewEvalContributionMinter(db evalMinterDB, ledger ledgerCreditTx, ratePerPoint float64, enabled func() bool) *EvalContributionMinter {
	m := &EvalContributionMinter{
		db:         db,
		ledger:     ledger,
		enabled:    enabled,
		minGraders: DefaultMinUnlinkedGraders,
		holdWindow: 72 * time.Hour,
	}
	if a, ok := NewHeldBenchmarkAnchor(ratePerPoint); ok {
		m.anchor = a // positive rate ⇒ live; otherwise nil ⇒ inert
	}
	return m
}

// SetHoldbackWindow overrides the 72h held→finalizable delay (non-positive keeps the default).
func (m *EvalContributionMinter) SetHoldbackWindow(d time.Duration) {
	if m != nil && d > 0 {
		m.holdWindow = d
	}
}

// SetMinUnlinkedGraders overrides the warmup floor (non-positive keeps the default). Lower is only for
// tests; production keeps DefaultMinUnlinkedGraders.
func (m *EvalContributionMinter) SetMinUnlinkedGraders(n int) {
	if m != nil && n > 0 {
		m.minGraders = n
	}
}

// RunOnce mints every currently mint-eligible item, each in its OWN claim-then-credit transaction.
// Total no-op BEFORE any DB access when inert (nil anchor / flag off / missing deps). Per-item failures
// (e.g. ErrEarnNotVerified for an unverified author) are logged and skipped — the item stays un-minted
// and re-eligible next tick (the U6 floor DELAYS, never forfeits).
func (m *EvalContributionMinter) RunOnce(ctx context.Context) (int, error) {
	if m == nil || m.anchor == nil || m.enabled == nil || !m.enabled() || m.db == nil || m.ledger == nil {
		return 0, nil // INERT: rate-0 (nil anchor) or flags off ⇒ no DB access, no mint
	}
	rows, err := m.db.Query(ctx, evalScanSQL)
	if err != nil {
		return 0, err
	}
	type cand struct{ itemID, authorWS string }
	cands := make([]cand, 0, 16)
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.itemID, &c.authorWS); err != nil {
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
		ok, err := m.mintOne(ctx, c.itemID, c.authorWS)
		if err != nil {
			slog.Warn("poolroyalty: eval-contribution mint failed (item stays un-minted; retries next tick)",
				slog.String("author", c.authorWS), slog.String("error", err.Error()))
			continue
		}
		if ok {
			minted++
		}
	}
	if len(cands) == evalSweepBatchLimit {
		slog.Info("poolroyalty: eval-contribution sweep hit batch limit — more items mint next tick",
			slog.Int("batch", evalSweepBatchLimit))
	}
	return minted, nil
}

// mintOne evaluates one item's discrimination and, if it clears the warmup with positive value, claims
// + credits it ONCE. Returns (true,nil) on a fresh mint; (false,nil) on a no-op (sub-warmup, non-
// discriminating, already-claimed, non-positive amount); (false,err) on a failure that rolls the item
// back for a later retry. The author's OWN nodes and fingerprint-linked workspaces are excluded from
// the discrimination read (evalDiscriminationSQL), so an author cannot grade their own item's score.
func (m *EvalContributionMinter) mintOne(ctx context.Context, itemID, authorWS string) (bool, error) {
	if itemID == "" || authorWS == "" {
		return false, nil
	}

	tx, err := m.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("poolroyalty: eval begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Discrimination over DISTINCT UNLINKED graders (author + linked set excluded). Read-only.
	var graders int
	var varPop float64
	if err := tx.QueryRow(ctx, evalDiscriminationSQL, itemID, authorWS).Scan(&graders, &varPop); err != nil {
		return false, fmt.Errorf("poolroyalty: eval discrimination read: %w", err)
	}
	// Warmup floor: below N distinct unlinked graders, an item pays NOTHING (no claim → re-eligible as
	// more graders arrive). One sock barely moves a variance over ≥N distinct unlinked workspaces.
	// RESIDUAL: a different-card/no-card sock evades the linked-set exclusion above and can inflate the
	// spread; bounded (NOT eliminated) by this floor + the U6 24h author cap — a logged pre-public-mint gate.
	if graders < m.minGraders {
		return false, nil
	}
	// discrimination = clamp01(4 · Var). The anchor clamps again; we clamp here so the AUDIT value
	// (stored in eval_contribution_mints) is the true [0,1] score.
	discrimination := 4 * varPop
	if discrimination < 0 {
		discrimination = 0
	}
	if discrimination > 1 {
		discrimination = 1
	}
	valuation := m.anchor.Value(GainInput{HeldScore: discrimination}) // rate × clamp01(discrimination) (Tier-2 float LENS)
	if math.IsNaN(valuation) || math.IsInf(valuation, 0) || valuation <= 0 {
		return false, nil // non-discriminating (Var 0) or rate-0 ⇒ nothing to mint, no claim, re-eligible
	}
	amount := microFloorLENS(valuation) // SEC-2 site #4: mint valuation → µLENS, rounded DOWN
	if amount <= 0 {
		return false, nil // sub-µLENS valuation ⇒ nothing to mint, no claim, re-eligible
	}

	// (1) Claim the item ONCE. ON CONFLICT DO NOTHING + RowsAffected is the exactly-once guard.
	tag, err := tx.Exec(ctx, evalInsertClaimSQL, itemID, authorWS, discrimination, graders, amount, m.holdWindow.Microseconds())
	if err != nil {
		return false, fmt.Errorf("poolroyalty: eval insert claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // already claimed — exactly-once suppression, no credit
	}

	// (2) Credit the AUTHOR's held balance — reached ONLY on a fresh claim. CreditHeldTx → heldInner →
	//     verifyEarn(author) [U6 floor] + reputation-bond code path (no-op for this non-bonded type) +
	//     checkMintRateCap(author) [the SAME 1000-LENS/24h cap; eval_contribution_held ∈ mintTypeList].
	//     An unverified author → ErrEarnNotVerified rolls back claim+credit; the item re-mints once verified.
	meta := map[string]interface{}{
		"request_id":       itemID,
		"source":           "eval_contribution",
		"anchor_kind":      m.anchor.Kind(),
		"discrimination":   discrimination,
		"distinct_graders": graders,
	}
	if err := m.ledger.CreditHeldTx(ctx, tx, authorWS, amount, TypeEvalContributionHeld,
		"proof-of-eval-contribution: discriminating eval item", meta); err != nil {
		return false, fmt.Errorf("poolroyalty: eval credit author (held): %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("poolroyalty: eval commit mint: %w", err)
	}
	return true, nil
}

// StartScheduler ticks RunOnce until ctx ends. Leader-elected at the call site (main wires it under
// haComps.leader.Run, like the distill mint/finalize workers). Inert until the rate + both flags are on.
func (m *EvalContributionMinter) StartScheduler(ctx context.Context, tick time.Duration) {
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
				slog.Warn("poolroyalty: eval-contribution mint sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}
