// distill_minter.go — L2/S4 PR3: the gated distill reuse-royalty mint.
//
// A leader-elected, flag-gated sweeper that mints s × avoided_cogs_usd to the
// OCR contributor (owner A), ONCE per cross-tenant reuse relationship, off the
// DEDUPLICATED distill_royalty_basis table (PR2) — never the per-serve fact
// stream. It mirrors the cache Minter's claim-then-act exactly-once shape and
// the FinalizeSweeper's per-row transaction discipline:
//
//	per un-minted relationship, in ONE transaction:
//	  (1) INSERT a claim into distill_royalty_mints ON CONFLICT (request_id)
//	      DO NOTHING — request_id = SHA256Hex(owner:requester:content_hash),
//	      the once-per-relationship key;
//	  (2) credit the contributor's HELD balance via CreditHeldTx ONLY IF the
//	      insert claimed a NEW row (RowsAffected == 1). A conflict (re-run /
//	      leader race / retry) inserts zero rows → NO credit (exactly-once).
//	Claim + credit commit or roll back together. An unverified-A credit
//	(CreditHeldTx → verifyEarn → ErrEarnNotVerified) rolls the WHOLE tx back, so
//	the claim row is discarded and the relationship stays un-minted —
//	re-eligible once A verifies (the U6 floor DELAYS the mint, never forfeits it).
//
// Reuses the Pool-B ledger kernel unchanged: CreditHeldTx → applyTx/heldInner →
// verifyEarn(owner) + checkMintRateCap(owner) with TypePoolRoyaltyHeld in
// mintTypeList, so the U6 verified-floor on the RECIPIENT, the per-identity rate
// cap, the 72h holdback, and supply-at-finalize all apply with no new ledger
// code. The distill FinalizeSweeper (NewFinalizeSweeper parameterized by table)
// settles the held rows. Inert by default: a nil/disabled minter is a total
// no-op, and the mint flag is read per tick BEFORE any DB access.
package poolroyalty

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// distillSweepBatchLimit bounds one tick's mint work so a backlog cannot stall
// the scheduler — rows past it mint on the next tick. NOT a silent cap: a full
// batch is logged.
const distillSweepBatchLimit = 500

// distillScanSQL finds un-minted reuse relationships: basis rows (0061) with NO
// claim row yet, via a LEFT JOIN on the relationship key. Leaves
// distill_royalty_basis untouched — the mint state lives entirely in
// distill_royalty_mints (a rolled-back / never-attempted relationship has no
// claim row, so it re-appears here until it actually commits a mint).
var distillScanSQL = fmt.Sprintf(`SELECT b.owner_workspace_id, b.requester_workspace_id, b.content_hash, b.avoided_cogs_usd
FROM distill_royalty_basis b
LEFT JOIN distill_royalty_mints m
       ON m.contributor_workspace_id = b.owner_workspace_id
      AND m.requester_workspace_id   = b.requester_workspace_id
      AND m.content_hash             = b.content_hash
WHERE m.request_id IS NULL
LIMIT %d`, distillSweepBatchLimit)

// distillInsertClaimSQL claims one relationship. ON CONFLICT (request_id) DO
// NOTHING + the caller's RowsAffected check is the exactly-once guard (the
// pool_royalty_mints / povi_challenges pattern). status='held'; finalize_after =
// now + holdback. No cache columns.
const distillInsertClaimSQL = `INSERT INTO distill_royalty_mints
    (request_id, contributor_workspace_id, requester_workspace_id, content_hash, avoided_cogs_usd, minted_amount, status, finalize_after)
VALUES ($1, $2, $3, $4, $5, $6, 'held', now() + ($7::bigint * interval '1 microsecond'))
ON CONFLICT (request_id) DO NOTHING`

// distillPairCapCountSQL / distillContentCapCountSQL are the PR1 per-pair and
// per-content mint caps — mirroring the cache minter's capCountSQL/entryCountSQL
// (minter.go:135/149) but over distill_royalty_mints (a SEPARATE budget; separate
// table). Counted INSIDE the mint tx AFTER CreditHeldTx, so each count rides the
// owner-balance FOR UPDATE the credit just took — concurrent mints for the same
// owner serialize there, making the count exact (a content_hash maps to ONE owner,
// so per-content is exact too). NO status filter — held+final+REVOKED all count, so
// revoking a mint never REFUNDS cap budget (mirrors the cache cap; the opposite of
// the detectors). The count includes the just-inserted claim row, so n > cap means
// "this would be the (cap+1)th".
const distillPairCapCountSQL = `SELECT COUNT(*) FROM distill_royalty_mints
WHERE contributor_workspace_id = $1 AND requester_workspace_id = $2
  AND created_at > now() - ($3::bigint * interval '1 microsecond')`

const distillContentCapCountSQL = `SELECT COUNT(*) FROM distill_royalty_mints
WHERE content_hash = $1
  AND created_at > now() - ($2::bigint * interval '1 microsecond')`

// distillMinterDB is the DB surface the sweeper needs: scan basis (Query) +
// a per-relationship transaction (Begin). *pgxpool.Pool satisfies it.
type distillMinterDB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// DistillMinter is the flag-gated distill reuse-royalty mint sweeper. The
// zero/nil minter is inert.
type DistillMinter struct {
	db             distillMinterDB
	ledger         ledgerCreditTx
	share          float64
	enabled        func() bool
	holdWindow     time.Duration
	linkageEnabled bool
	capPerPair     int           // PR1: max mints per (owner, requester) pair in capWindow; 0 = off
	capPerContent  int           // PR1: max mints per content_hash in capWindow; 0 = off
	capWindow      time.Duration // rolling window both caps count over (default 24h)
}

// NewDistillMinter wires the pool, the held ledger, the contributor share s (the
// SAME cfg.PoolRoyaltyShare as the cache royalty), and the per-tick mint flag.
// share is clamped to [0,1] (NaN/negative → 0, >1 → 1) — the Burn-and-Mint
// invariant. holdWindow defaults to 72h.
func NewDistillMinter(db distillMinterDB, ledger ledgerCreditTx, share float64, enabled func() bool) *DistillMinter {
	if math.IsNaN(share) || share < 0 {
		share = 0
	}
	if share > 1 {
		share = 1
	}
	return &DistillMinter{db: db, ledger: ledger, share: share, enabled: enabled, holdWindow: 72 * time.Hour, capWindow: 24 * time.Hour}
}

// SetOwnerLinkageCheck enables the U6 PR2 owner-linkage wash guard: deny a
// distill mint between two workspaces sharing a captured card fingerprint
// (default-allow on missing). Mirrors the cache Minter.
func (m *DistillMinter) SetOwnerLinkageCheck(enabled bool) {
	if m != nil {
		m.linkageEnabled = enabled
	}
}

// SetHoldbackWindow overrides the 72h default (non-positive keeps the default).
func (m *DistillMinter) SetHoldbackWindow(d time.Duration) {
	if m != nil && d > 0 {
		m.holdWindow = d
	}
}

// SetCap sets the per-(owner, requester) mint cap over the window: at most perPair
// distill mints for one pair within the window. perPair <= 0 disables (the default).
// window <= 0 keeps the current window. Mirrors the cache Minter.SetCap. Deflationary:
// a cap can only DENY a mint, never create one.
func (m *DistillMinter) SetCap(perPair int, window time.Duration) {
	if m == nil {
		return
	}
	if perPair < 0 {
		perPair = 0
	}
	m.capPerPair = perPair
	if window > 0 {
		m.capWindow = window
	}
}

// SetContentCap sets the per-content_hash mint cap over the window: at most
// perContent distill mints for one document across ALL requesters. perContent <= 0
// disables (the default). Shares the SetCap window. Mirrors Minter.SetEntryCap.
func (m *DistillMinter) SetContentCap(perContent int, window time.Duration) {
	if m == nil {
		return
	}
	if perContent < 0 {
		perContent = 0
	}
	m.capPerContent = perContent
	if window > 0 {
		m.capWindow = window
	}
}

type distillRelationship struct {
	owner          string
	requester      string
	contentHash    string
	avoidedCOGSUSD float64
}

// RunOnce mints every currently un-minted relationship, each in its OWN
// claim-then-credit transaction. Returns the count minted. Flag-off / nil /
// missing deps is a no-op BEFORE any DB access. Per-relationship failures (e.g.
// ErrEarnNotVerified for an unverified contributor) are logged and skipped — the
// relationship stays un-minted (its tx rolled back) and is re-eligible next tick.
func (m *DistillMinter) RunOnce(ctx context.Context) (int, error) {
	if m == nil || m.enabled == nil || !m.enabled() || m.db == nil || m.ledger == nil {
		return 0, nil
	}
	rows, err := m.db.Query(ctx, distillScanSQL)
	if err != nil {
		return 0, err
	}
	rels := make([]distillRelationship, 0, 16)
	for rows.Next() {
		var r distillRelationship
		if err := rows.Scan(&r.owner, &r.requester, &r.contentHash, &r.avoidedCOGSUSD); err != nil {
			rows.Close()
			return 0, err
		}
		rels = append(rels, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	minted := 0
	for _, r := range rels {
		ok, err := m.mintOne(ctx, r)
		if err != nil {
			slog.Warn("poolroyalty: distill mint failed (relationship stays un-minted; retries next tick)",
				slog.String("contributor", r.owner),
				slog.String("error", err.Error()))
			continue
		}
		if ok {
			minted++
		}
	}
	if len(rels) == distillSweepBatchLimit {
		slog.Info("poolroyalty: distill mint sweep hit batch limit — more relationships mint next tick",
			slog.Int("batch", distillSweepBatchLimit))
	}
	return minted, nil
}

// mintOne claims and credits ONE relationship in a single transaction. Returns
// (true, nil) on a fresh mint; (false, nil) on a deflationary no-op (already
// minted / linked / self-serve / non-positive amount); (false, err) on a failure
// that leaves the relationship un-minted (rolled back) for a later retry —
// INCLUDING an unverified contributor (ErrEarnNotVerified): the claim row is
// discarded with the credit, so the U6 floor DELAYS the mint, never forfeits it.
func (m *DistillMinter) mintOne(ctx context.Context, r distillRelationship) (bool, error) {
	// Deflationary guards (no-op, never a half-mint): a self-serve, malformed
	// relationship, or a non-finite / non-positive amount must never mint.
	if r.owner == "" || r.requester == "" || r.owner == r.requester {
		return false, nil
	}
	amount := m.share * r.avoidedCOGSUSD
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return false, nil
	}
	requestID := SHA256Hex([]byte(r.owner + ":" + r.requester + ":" + r.contentHash))

	tx, err := m.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("poolroyalty: distill begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// U6 PR2 owner-linkage: deny a mint between two workspaces sharing a captured
	// card fingerprint (default-allow on missing). Read-only, before the claim +
	// the credit's balance FOR UPDATE — no lock-ordering surface.
	if m.linkageEnabled {
		var linked bool
		if err := tx.QueryRow(ctx, sharedFingerprintSQL, r.owner, r.requester).Scan(&linked); err != nil {
			return false, fmt.Errorf("poolroyalty: distill owner-linkage check: %w", err)
		}
		if linked {
			return false, nil // deflationary no-op
		}
	}

	// (1) Claim the relationship. ON CONFLICT DO NOTHING + RowsAffected is the
	//     exactly-once guard: a re-run / leader race / retry inserts ZERO rows.
	tag, err := tx.Exec(ctx, distillInsertClaimSQL,
		requestID, r.owner, r.requester, r.contentHash, r.avoidedCOGSUSD, amount, m.holdWindow.Microseconds())
	if err != nil {
		return false, fmt.Errorf("poolroyalty: distill insert claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Already claimed — exactly-once suppression. NO credit; the deferred
		// rollback ends the tx. (This is the conflict path: it must NOT credit.)
		return false, nil
	}

	// (2) Credit the contributor's HELD balance — reached ONLY when the claim took
	//     a NEW row. CreditHeldTx → verifyEarn(owner): an unverified contributor
	//     returns ErrEarnNotVerified and the WHOLE tx (claim + credit) rolls back,
	//     leaving the relationship un-minted and re-eligible once it verifies. The
	//     requester is DELIBERATELY omitted from the contributor's ledger row
	//     (#145: tokens/history echoes it to the contributor — naming the
	//     counterparty there would leak cross-tenant identity); it stays in the
	//     admin-only claim row above.
	meta := map[string]interface{}{
		"request_id":       requestID,
		"source":           "distill_ocr_reuse",
		"content_hash":     r.contentHash,
		"avoided_cogs_usd": r.avoidedCOGSUSD,
		"royalty_share":    m.share,
	}
	if err := m.ledger.CreditHeldTx(ctx, tx, r.owner, amount, TypePoolRoyaltyHeld,
		"distill reuse royalty: cross-tenant OCR transcription served", meta); err != nil {
		return false, fmt.Errorf("poolroyalty: distill credit contributor (held): %w", err)
	}

	// (3) PR1 caps — counted AFTER the credit (rides the owner-balance FOR UPDATE the
	//     credit just took, so the count is exact under concurrency, like the cache
	//     minter). Over the cap → the deferred rollback discards the claim + credit: a
	//     CAPPED mint is a deflationary no-op (return false, nil — NOT an error, so no
	//     per-tick log spam), and the relationship re-eligibly mints once the window
	//     frees budget. Both counts include the just-inserted held row and count
	//     revoked (a revoke never refunds budget).
	if m.capPerPair > 0 {
		var n int64
		if err := tx.QueryRow(ctx, distillPairCapCountSQL, r.owner, r.requester, m.capWindow.Microseconds()).Scan(&n); err != nil {
			return false, fmt.Errorf("poolroyalty: distill pair cap count: %w", err)
		}
		if n > int64(m.capPerPair) {
			return false, nil // per-pair cap reached — rollback, no mint
		}
	}
	if m.capPerContent > 0 {
		var n int64
		if err := tx.QueryRow(ctx, distillContentCapCountSQL, r.contentHash, m.capWindow.Microseconds()).Scan(&n); err != nil {
			return false, fmt.Errorf("poolroyalty: distill content cap count: %w", err)
		}
		if n > int64(m.capPerContent) {
			return false, nil // per-content cap reached — rollback, no mint
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("poolroyalty: distill commit mint: %w", err)
	}
	return true, nil
}

// StartScheduler ticks RunOnce until ctx ends — mirrors the cache finalize
// scheduler. Leader-elected at the call site (main wires it under
// haComps.leader.Run, like the cache mint/finalize workers).
func (m *DistillMinter) StartScheduler(ctx context.Context, tick time.Duration) {
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
				slog.Warn("poolroyalty: distill mint sweep failed", slog.String("error", err.Error()))
			}
		}
	}
}
