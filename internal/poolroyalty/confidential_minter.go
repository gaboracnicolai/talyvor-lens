// confidential_minter.go — Proof-of-Improvement instance 4: proof-of-confidential-compute (the CONFIDENTIAL
// COMPUTE MINT). Pays a NODE a FLAT per-epoch reward for providing VERIFIED confidential capacity — the
// scarce, cryptographically-verified property — through the SAME held-ledger / U6 chokepoint. The 4th live
// HeldBenchmarkAnchor caller. Epoch-settled once per (node, attested_gpu_class, 24h window).
//
// WHAT IT PAYS: verified confidential capacity, NOT latency/volume. The latency mint (§A26) already pays
// latency; re-paying it here would double-count — so this minter reads NO latency signal at all. The reward
// is FLAT: amount = anchor.Value(HeldScore: 1.0) = rate × 1.0. attested_gpu_class is the gateway-VERIFIED
// class (from the NVIDIA EAT, step b); the mint NEVER reads the node-declared gpu_type.
//
// ELIGIBILITY (all AND-ed in confidentialScanSQL): (i) node_attestations status='verified' AND
// key_bound=true AND expires_at > now() — key_bound=true is the RELAY FENCE, so relay-vulnerable rows are
// excluded by the WHERE clause; (ii) held-probe correctness — EXISTS a benchmark_node_scores row for the
// node with score >= qualityThreshold AND sample_count >= qualityWarmup (the same node-blind C-gate the
// latency mint uses, at the per-node grain since attestation is per-node hardware).
//
// NO-LOOP: reads node_attestations ⋈ inference_nodes ⋈ benchmark_node_scores by RAW SQL; writes only
// confidential_compute_mints + the ledger. Imports NONE of attestation/nodelatency/benchprobe/proxy
// (asserted by confidential_noloop_test.go). Minting cannot change a future attestation or benchmark score.
//
// INERT BY DEFAULT: rate 0 ⇒ NewHeldBenchmarkAnchor refuses ⇒ nil anchor ⇒ RunOnce is a TOTAL no-op even
// with both flags on. ALSO inert-by-substrate-absence: no key_bound=true row exists until real CC hardware +
// enclave report_data binding is deployed, so RunOnce mints ZERO even flag+rate on (why this ships
// merge-held). Not reputation-bonded; still U6 floor + 24h cap via mintTypeList (TypeConfidentialComputeHeld).
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
	// confidentialCapacityValue is the FLAT held-score for verified confidential capacity — binary: a node
	// either provides gateway-verified CC capacity or it doesn't. amount = rate × clamp01(1.0) = rate.
	confidentialCapacityValue               = 1.0
	confidentialMintBatchSize               = 500
	confidentialMintDefaultWindowSeconds    = 86400 // 24h settlement window
	confidentialMintDefaultQualityThreshold = 0.7   // benchmark_node_scores.score floor (reuse latency C-gate)
	confidentialMintDefaultQualityWarmup    = 5     // benchmark_node_scores.sample_count floor
)

// confidentialScanSQL returns each eligible (node, attested_gpu_class) not yet claimed this epoch: a VALID
// attestation (verified + key_bound + unexpired) whose node clears the per-node held-probe C-gate. $1=epoch,
// $2=qualityThreshold, $3=qualityWarmup, $4=batch. Reads NO gpu_type, NO latency table.
const confidentialScanSQL = `WITH valid_attestation AS (
    SELECT DISTINCT node_id, attested_gpu_class
    FROM node_attestations
    WHERE attestation_status = 'verified' AND key_bound = true AND expires_at > now()
)
SELECT a.node_id, n.workspace_id, a.attested_gpu_class
FROM valid_attestation a
JOIN inference_nodes n ON n.id = a.node_id
WHERE EXISTS (
        SELECT 1 FROM benchmark_node_scores b
        WHERE b.node_id = a.node_id AND b.score >= $2 AND b.sample_count >= $3)
  AND NOT EXISTS (
        SELECT 1 FROM confidential_compute_mints m
        WHERE m.node_id = a.node_id AND m.attested_gpu_class = a.attested_gpu_class AND m.epoch = $1)
LIMIT $4`

// confidentialInsertClaimSQL claims one (node, class, epoch) ONCE. ON CONFLICT (request_id) DO NOTHING +
// RowsAffected is the exactly-once-per-window guard. Generic finalize columns ⇒ generic FinalizeSweeper.
const confidentialInsertClaimSQL = `INSERT INTO confidential_compute_mints
    (request_id, contributor_workspace_id, minted_amount, node_id, attested_gpu_class, epoch, status, finalize_after)
VALUES ($1, $2, $3, $4, $5, $6, 'held', now() + ($7::bigint * interval '1 microsecond'))
ON CONFLICT (request_id) DO NOTHING`

type confidentialMinterDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ConfidentialMinter is the flag-gated proof-of-confidential-compute mint sweeper. A nil anchor (rate-0
// default) makes it inert.
type ConfidentialMinter struct {
	db               confidentialMinterDB
	ledger           ledgerCreditTx
	enabled          func() bool
	anchor           Anchor // HeldBenchmarkAnchor{rate}; nil ⇒ inert. The FOURTH live held-benchmark anchor.
	holdWindow       time.Duration
	windowSeconds    int64
	qualityThreshold float64
	qualityWarmup    int
	batchSize        int
	now              func() time.Time
}

// NewConfidentialMinter builds the sweeper. ratePerPoint is the LENS-per-epoch flat capacity rate; REQUIRED
// — NewHeldBenchmarkAnchor refuses 0/neg/NaN/Inf, so the shipped default 0 leaves anchor nil and the minter
// a TOTAL no-op. This is the FOURTH sanctioned non-test caller of NewHeldBenchmarkAnchor (reachability guard).
func NewConfidentialMinter(db confidentialMinterDB, ledger ledgerCreditTx, ratePerPoint float64, enabled func() bool) *ConfidentialMinter {
	m := &ConfidentialMinter{
		db: db, ledger: ledger, enabled: enabled,
		holdWindow:       72 * time.Hour,
		windowSeconds:    confidentialMintDefaultWindowSeconds,
		qualityThreshold: confidentialMintDefaultQualityThreshold,
		qualityWarmup:    confidentialMintDefaultQualityWarmup,
		batchSize:        confidentialMintBatchSize,
		now:              time.Now,
	}
	if a, ok := NewHeldBenchmarkAnchor(ratePerPoint); ok {
		m.anchor = a
	}
	return m
}

// SetHoldbackWindow overrides the 72h held→finalizable delay (non-positive keeps the default).
func (m *ConfidentialMinter) SetHoldbackWindow(d time.Duration) {
	if m != nil && d > 0 {
		m.holdWindow = d
	}
}

type confidentialRow struct {
	nodeID, workspaceID, gpuClass string
}

// RunOnce mints every currently-eligible (node, class) for the current epoch, each in its own claim-then-
// credit transaction. Total no-op BEFORE any DB access when inert.
func (m *ConfidentialMinter) RunOnce(ctx context.Context) (int, error) {
	if m == nil || m.anchor == nil || m.enabled == nil || !m.enabled() || m.db == nil || m.ledger == nil {
		return 0, nil // INERT: rate-0 (nil anchor) or flags off ⇒ no DB access, no mint
	}
	epoch := m.now().Unix() / m.windowSeconds

	rows, err := m.db.Query(ctx, confidentialScanSQL, epoch, m.qualityThreshold, m.qualityWarmup, m.batchSize)
	if err != nil {
		return 0, err
	}
	cands := make([]confidentialRow, 0, 16)
	for rows.Next() {
		var r confidentialRow
		if err := rows.Scan(&r.nodeID, &r.workspaceID, &r.gpuClass); err != nil {
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
			slog.Warn("poolroyalty: confidential mint failed (row stays un-minted; retries next tick)",
				slog.String("node", r.nodeID), slog.String("workspace", r.workspaceID), slog.String("error", err.Error()))
			continue
		}
		if ok {
			minted++
		}
	}
	if len(cands) == m.batchSize {
		slog.Info("poolroyalty: confidential mint sweep hit batch limit — more nodes mint next tick",
			slog.Int("batch", m.batchSize))
	}
	return minted, nil
}

func (m *ConfidentialMinter) mintOne(ctx context.Context, epoch int64, r confidentialRow) (bool, error) {
	if r.nodeID == "" || r.workspaceID == "" || r.gpuClass == "" {
		return false, nil
	}
	amount := m.anchor.Value(GainInput{HeldScore: confidentialCapacityValue})
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return false, nil // rate-0 ⇒ no claim, no mint
	}
	requestID := confidentialRequestID(r.nodeID, r.gpuClass, epoch)

	tx, err := m.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("poolroyalty: confidential begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, confidentialInsertClaimSQL,
		requestID, r.workspaceID, amount, r.nodeID, r.gpuClass, epoch, m.holdWindow.Microseconds())
	if err != nil {
		return false, fmt.Errorf("poolroyalty: confidential insert claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // already claimed this epoch — exactly-once suppression
	}

	meta := map[string]interface{}{
		"node_id":            r.nodeID,
		"source":             "confidential_compute",
		"anchor_kind":        m.anchor.Kind(),
		"attested_gpu_class": r.gpuClass,
		"epoch":              epoch,
	}
	if err := m.ledger.CreditHeldTx(ctx, tx, r.workspaceID, amount, TypeConfidentialComputeHeld,
		"proof-of-confidential-compute: verified confidential capacity", meta); err != nil {
		return false, fmt.Errorf("poolroyalty: confidential credit node-workspace (held): %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("poolroyalty: confidential commit mint: %w", err)
	}
	return true, nil
}

// StartScheduler ticks RunOnce until ctx ends. Leader-elected at the call site. Inert until the rate + both
// flags are on AND a key_bound=true attestation exists.
func (m *ConfidentialMinter) StartScheduler(ctx context.Context, tick time.Duration) {
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
				slog.Warn("poolroyalty: confidential mint sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}

// confidentialRequestID is the deterministic once-per-window key: SHA256Hex(node_id:attested_gpu_class:epoch).
func confidentialRequestID(nodeID, gpuClass string, epoch int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", nodeID, gpuClass, epoch)))
	return hex.EncodeToString(h[:])
}
