// Package poolroyalty implements Phase-2 Stage 2.1: the Pool-B royalty mint —
// a served cross-tenant pooled cache hit mints s × avoided_COGS to the
// contributing tenant, EXACTLY ONCE per serving request.
//
// Idempotency is the claim-then-act shape from povi_challenges (the
// double-slash guard): INSERT a claim row into pool_royalty_mints ON CONFLICT
// (request_id) DO NOTHING, and proceed to the ledger credit ONLY when the
// insert claimed the row (RowsAffected == 1) — claim and credit join ONE
// transaction so they commit or roll back together. request_id ALONE is the
// unique key: a retried request can legitimately re-match a DIFFERENT
// semantic entry, so keying on the match (entry/contributor) would
// reintroduce the double-mint. A colliding request_id can only SUPPRESS a
// later mint (deflationary, safe) — never inflate supply.
//
// The package extends the existing ledger primitives (CreditTx → applyTx's
// two-step FOR UPDATE balance lock — seam #1's one locking discipline); it
// adds no new lock and no advisory lock: the UNIQUE constraint is the
// cross-instance serialization point. All statements are plain parameterized
// SQL (PgBouncer transaction-pooling / simple-protocol safe; no session
// state). Inert by default: a nil/disabled Minter is a total no-op.
package poolroyalty

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/metrics"
)

// TypePoolRoyalty tags a Pool-B royalty mint in the lens_token_ledger. Like
// the other mint types it is credit-side; supply dashboards gain it when the
// flag flips on (see the Stage-2.1 flip-on notes in COORDINATION.md).
const TypePoolRoyalty = "pool_royalty"

// DefaultRoyaltyShare is the default contributor share s of avoided_COGS
// (LENS_POOL_ROYALTY_SHARE overrides; clamped to [0,1]). With share s the
// contributor mints s × avoided_COGS and Talyvor nets (1−s) × avoided_COGS,
// which the clamp keeps ≥ 0 (the Burn-and-Mint invariant).
const DefaultRoyaltyShare = 0.5

// ServedHit describes one SERVED cross-tenant pooled cache hit — the caller
// must only construct it at the serve point (after the cached body actually
// went out), never at lookup: a found-but-not-served hit must not mint.
type ServedHit struct {
	RequestID            string  // the serving request's X-Talyvor-Request-ID (the idempotency key)
	RequesterWorkspace   string  // who was served
	ContributorWorkspace string  // who contributed the entry (owner stamp)
	Layer                string  // "exact" | "semantic"
	EntryID              string  // exact: pooled cache key; semantic: prompt_embeddings.id — attribution DATA, not key
	Provider             string
	Model                string
	Similarity           float64 // semantic hits only; 0 for exact
	AvoidedCOGSUSD       float64 // what the requester's live call would have cost (estimated-tokens semantics)
}

// Result is the outcome of one mint attempt.
type Result struct {
	Minted        bool    // a claim was taken and the contributor credited
	AlreadyMinted bool    // the request_id was already claimed — exactly-once suppression
	Amount        float64 // LENS credited (s × avoided_COGS) when Minted
}

// txBeginner matches *pgxpool.Pool.Begin (the povi/stakes.go seam) so tests
// can inject pgxmock and a nil DB degrades to a no-op.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ledgerCreditTx is the minimal composable-credit surface the mint needs;
// *mining.LedgerStore satisfies it exactly, so this package stays decoupled
// from the ledger package (and tests pass a fake).
type ledgerCreditTx interface {
	CreditTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount float64, txType, description string, metadata map[string]interface{}) error
}

// insertClaimSQL claims the serving request: ON CONFLICT (request_id) DO
// NOTHING + a RowsAffected check is the exactly-once guard (povi_challenges
// pattern). id and created_at take their column defaults.
const insertClaimSQL = `INSERT INTO pool_royalty_mints
    (request_id, requester_workspace_id, contributor_workspace_id, layer, entry_id, provider, model, similarity, avoided_cogs_usd, minted_amount)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (request_id) DO NOTHING`

// Minter mints s × avoided_COGS to the contributor, exactly once per serving
// request. Construct via NewMinter; the zero/nil Minter is inert.
type Minter struct {
	db      txBeginner
	ledger  ledgerCreditTx
	share   float64
	enabled func() bool
}

// NewMinter builds a Minter. share is clamped to [0,1] so the Burn-and-Mint
// invariant (Talyvor nets (1−s) × avoided_COGS ≥ 0) cannot be violated by
// config. enabled is read per call so the flag stays live without rewiring.
func NewMinter(db txBeginner, ledger ledgerCreditTx, share float64, enabled func() bool) *Minter {
	if share < 0 {
		share = 0
	}
	if share > 1 {
		share = 1
	}
	return &Minter{db: db, ledger: ledger, share: share, enabled: enabled}
}

// Share returns the clamped contributor royalty share s.
func (m *Minter) Share() float64 {
	if m == nil {
		return 0
	}
	return m.share
}

// MintServedHit claims the serving request and credits the contributor in ONE
// transaction (Andrew constraint #1). Exactly-once: a request_id that was
// already claimed returns AlreadyMinted with NO ledger write. No-ops (never
// errors) when: disabled, nil receiver/deps, empty request_id or contributor,
// a self-serve (requester == contributor), or a non-positive mint amount —
// every defensive direction is deflationary.
func (m *Minter) MintServedHit(ctx context.Context, h ServedHit) (Result, error) {
	if m == nil || m.enabled == nil || !m.enabled() || m.db == nil || m.ledger == nil {
		return Result{}, nil
	}
	if h.RequestID == "" || h.ContributorWorkspace == "" || h.ContributorWorkspace == h.RequesterWorkspace {
		return Result{}, nil
	}
	amount := m.share * h.AvoidedCOGSUSD
	if amount <= 0 {
		return Result{}, nil
	}

	tx, err := m.db.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("poolroyalty: begin mint tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, insertClaimSQL,
		h.RequestID, h.RequesterWorkspace, h.ContributorWorkspace, h.Layer,
		h.EntryID, h.Provider, h.Model, h.Similarity, h.AvoidedCOGSUSD, amount)
	if err != nil {
		return Result{}, fmt.Errorf("poolroyalty: insert mint claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Another serve (or a retry) already claimed this request_id — the
		// exactly-once suppression. Nothing was written; the deferred
		// rollback just ends the transaction.
		return Result{AlreadyMinted: true}, nil
	}

	meta := map[string]interface{}{
		"request_id":           h.RequestID,
		"request_workspace_id": h.RequesterWorkspace, // CountCacheHitsBenefited's metadata convention
		"layer":                h.Layer,
		"entry_id":             h.EntryID,
		"avoided_cogs_usd":     h.AvoidedCOGSUSD,
		"royalty_share":        m.share,
	}
	desc := fmt.Sprintf("pool royalty: %s pooled hit served to %s", h.Layer, h.RequesterWorkspace)
	if err := m.ledger.CreditTx(ctx, tx, h.ContributorWorkspace, amount, TypePoolRoyalty, desc, meta); err != nil {
		return Result{}, fmt.Errorf("poolroyalty: credit contributor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("poolroyalty: commit mint: %w", err)
	}
	// A royalty mint is LENS entering circulation — mirror Credit()'s supply
	// metric (CreditTx deliberately doesn't emit it; the standalone Credit does).
	metrics.MintedTokens(amount)
	return Result{Minted: true, Amount: amount}, nil
}
