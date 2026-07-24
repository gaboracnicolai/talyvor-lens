// Package distillattrib is the S1 distill attribution store: a durable,
// MINT-FREE per-(owner, requester, artifact) counter of consented cross-tenant
// pooled-distill serves.
//
// STRUCTURAL INERTNESS: this package imports NO ledger and exposes NO
// credit/earn method. Its two writes — RecordDistillServe (the serve_count
// counter, migration 0052) and RecordRoyaltyBasis (the avoided-COGS FIGURE,
// migration 0061) — can only ever touch those two descriptive tables. Both are
// Exec-only on the `execer` surface (no Query/Begin), so neither can read or open
// a transaction, let alone touch a ledger. Recording a money FIGURE is not
// minting; attribution/basis are descriptive, never incentivized (WorkTier
// doctrine), and the gated royalty (L2/S4 PR3) is a separate build — keyed off
// the deduplicated distill_royalty_basis table — that this package cannot reach.
package distillattrib

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
)

// execer is the minimal DB surface the store needs — just Exec. *pgxpool.Pool
// satisfies it; tests inject a fake to assert the UPSERT shape without a DB.
// Deliberately Exec-only: no Query/Begin, so this store cannot read or open a
// transaction, let alone touch a ledger.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store records consented cross-tenant distill serves. Nil-safe: a nil store (or
// nil pool) is a no-op, so attribution is inert until a real pool is wired.
type Store struct {
	db execer
}

// NewStore builds a Store over an Exec-capable pool. Pass nil to keep
// attribution inert.
func NewStore(db execer) *Store {
	if db == nil {
		return nil
	}
	return &Store{db: db}
}

// upsertSQL is the durable per-tuple counter: one row per
// (owner, requester, content_hash); each consented serve bumps serve_count and
// refreshes last_served_at. Mirrors the cache layer's hit_count = hit_count + 1
// idiom — re-serving the same artifact increments, never appends. NO ledger, NO
// caps, NO status, NO request_id claim.
const upsertSQL = `INSERT INTO distill_serve_attribution
  (owner_workspace_id, requester_workspace_id, content_hash, serve_count)
VALUES ($1, $2, $3, 1)
ON CONFLICT (owner_workspace_id, requester_workspace_id, content_hash)
DO UPDATE SET serve_count    = distill_serve_attribution.serve_count + 1,
              last_served_at = NOW()`

// RecordDistillServe records (or increments) one consented cross-tenant serve.
// Returns only an error, for the caller to log-and-swallow — it never mints,
// credits, or writes anything but the attribution counter. Nil-safe.
func (s *Store) RecordDistillServe(ctx context.Context, owner, requester, contentHash string) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.Exec(ctx, upsertSQL, owner, requester, contentHash)
	return err
}

// basisInsertSQL records the avoided-COGS BASIS once per cross-tenant OCR reuse
// relationship (owner, requester, content_hash). ON CONFLICT DO NOTHING PINS the
// first-captured basis: a re-serve never overwrites it, so the figure PR3 mints
// against is stable per relationship even if the cached model/cost later changes.
// A descriptive money FIGURE — still NO ledger, NO mint (this package imports
// neither; see the structural-inertness note above).
const basisInsertSQL = `INSERT INTO distill_royalty_basis
  (owner_workspace_id, requester_workspace_id, content_hash,
   avoided_cogs_usd, vision_model, vision_input_tokens, vision_output_tokens)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (owner_workspace_id, requester_workspace_id, content_hash) DO NOTHING`

// RecordRoyaltyBasis records the avoided-COGS basis for ONE consented cross-tenant
// OCR reuse relationship — once (ON CONFLICT DO NOTHING; a re-serve never
// overwrites the pinned figure). avoidedCOGS MUST be CostUSD(visionModel, inTokens,
// outTokens), computed at serve time from the cached OCR entry; the model + token
// split are stored as provenance so the figure is re-derivable. Returns only an
// error for the caller to log-and-swallow — it NEVER mints or credits (this store
// has no ledger surface). Nil-safe.
func (s *Store) RecordRoyaltyBasis(ctx context.Context, owner, requester, contentHash string, avoidedCOGS float64, visionModel string, inTokens, outTokens int) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.Exec(ctx, basisInsertSQL, owner, requester, contentHash, avoidedCOGS, visionModel, inTokens, outTokens)
	return err
}

// basisInsertFundedSQL is RecordRoyaltyBasis PLUS the settled_charge_usd (0105) — what the reuser was
// ACTUALLY charged for this reuse. Same ON CONFLICT DO NOTHING pinning: the first serve fixes the funding.
const basisInsertFundedSQL = `INSERT INTO distill_royalty_basis
  (owner_workspace_id, requester_workspace_id, content_hash,
   avoided_cogs_usd, settled_charge_usd, vision_model, vision_input_tokens, vision_output_tokens)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (owner_workspace_id, requester_workspace_id, content_hash) DO NOTHING`

// RecordRoyaltyBasisFunded records the basis WITH the consumer's settled charge — THE distill funding
// invariant (mirror of pool royalty #351). settledChargeUSD is the USD the reuser was actually billed for
// this reuse (the settled reservation, which resolveCacheReservation returns since #351). The async
// DistillMinter mints s × settled_charge and skips any basis whose charge is absent/≤0, so a royalty is
// funded by a real payment and an unfunded reuse mints nothing.
//
// THE SERVE-SIDE HANDOFF: recordDistillServes (internal/proxy) must call THIS instead of RecordRoyaltyBasis,
// passing the settled charge for the reuse request. Until it does, basis rows carry NULL charge and mint
// nothing — the fail-closed default (no mint we cannot prove was funded). Nil-safe.
func (s *Store) RecordRoyaltyBasisFunded(ctx context.Context, owner, requester, contentHash string, avoidedCOGS, settledChargeUSD float64, visionModel string, inTokens, outTokens int) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.Exec(ctx, basisInsertFundedSQL, owner, requester, contentHash, avoidedCOGS, settledChargeUSD, visionModel, inTokens, outTokens)
	return err
}
