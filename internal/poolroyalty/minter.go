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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/mining"
)

// TypePoolRoyalty tags a Pool-B royalty mint in the lens_token_ledger. The
// canonical constant lives in internal/mining with the other ledger row
// types (and in GetTotalSupply's allow-list since Stage 2.2); this alias
// keeps the writer-side name local.
const TypePoolRoyalty = mining.TypePoolRoyalty

// TypePoolRoyaltyHeld is the mint-time HELD credit type (Stage 2.3a) —
// uncounted by supply; the counted TypePoolRoyalty row is written at
// finalize by the sweeper. Canonical constant lives in mining.
const TypePoolRoyaltyHeld = mining.TypePoolRoyaltyHeld

// TypeEvalContributionHeld is the proof-of-eval-contribution HELD mint type (Proof-of-Improvement
// instance 1) — like TypePoolRoyaltyHeld: gated by the U6 floor + 24h cap via mintTypeList, settled by
// the FinalizeSweeper. Canonical constant lives in mining.
const TypeEvalContributionHeld = mining.TypeEvalContributionHeld

// DefaultRoyaltyShare is the default contributor share s of avoided_COGS
// (LENS_POOL_ROYALTY_SHARE overrides; clamped to [0,1]). With share s the
// contributor mints s × avoided_COGS and Talyvor nets (1−s) × avoided_COGS,
// which the clamp keeps ≥ 0 (the Burn-and-Mint invariant).
const DefaultRoyaltyShare = 0.5

// ServedHit describes one SERVED cross-tenant pooled cache hit — the caller
// must only construct it at the serve point (after the cached body actually
// went out), never at lookup: a found-but-not-served hit must not mint.
type ServedHit struct {
	RequestID            string // the serving request's X-Talyvor-Request-ID (the idempotency key)
	RequesterWorkspace   string // who was served
	ContributorWorkspace string // who contributed the entry (owner stamp)
	Layer                string // "exact" | "semantic"
	EntryID              string // exact: pooled cache key; semantic: prompt_embeddings.id — attribution DATA, not key
	Provider             string
	Model                string
	Similarity           float64 // semantic hits only; 0 for exact
	AvoidedCOGSUSD       float64 // what the requester's live call would have cost (estimated-tokens semantics)

	// Stage 2.3.0 evidence hashes — UNSALTED hex(sha256(...)) over the raw
	// bytes, computed AT SERVE (both cache stores are mutable underneath the
	// mint; a later hash could bind different bytes). Empty means the serve
	// could not capture them (e.g. a none-LoggingPolicy requester) — and the
	// gate below then refuses to mint: an unadjudicable mint is never created.
	AnswerSHA256 string // hex(sha256(served response bytes))
	PromptSHA256 string // hex(sha256(raw requester prompt bytes))
}

// SHA256Hex is the house content hash: hex(sha256(b)), UNSALTED — no
// provider:model prefix (the salted identities already live on the claim row
// via entry_id). Used for both Stage-2.3.0 evidence hashes.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Result is the outcome of one mint attempt.
type Result struct {
	Minted        bool    // a claim was taken and the contributor credited
	AlreadyMinted bool    // the request_id was already claimed — exactly-once suppression
	Capped        bool    // a window cap was reached — claim+credit rolled back (2.3b)
	CapReason     string  // when Capped: "per_pair" | "per_entry" (which cap fired)
	Amount        float64 // LENS credited (s × avoided_COGS) when Minted
}

// txBeginner matches *pgxpool.Pool.Begin (the povi/stakes.go seam) so tests
// can inject pgxmock and a nil DB degrades to a no-op.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ledgerCreditTx is the minimal composable-credit surface the mint needs —
// since Stage 2.3a this is the HELD credit (the mint never writes spendable
// balance; the finalize sweeper does, after the holdback window).
// *mining.LedgerStore satisfies it exactly.
type ledgerCreditTx interface {
	CreditHeldTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount float64, txType, description string, metadata map[string]interface{}) error
}

// insertClaimSQL claims the serving request: ON CONFLICT (request_id) DO
// NOTHING + a RowsAffected check is the exactly-once guard (povi_challenges
// pattern). id and created_at take their column defaults.
const insertClaimSQL = `INSERT INTO pool_royalty_mints
    (request_id, requester_workspace_id, contributor_workspace_id, layer, entry_id, provider, model, similarity, avoided_cogs_usd, minted_amount, answer_sha256, prompt_sha256, status, finalize_after)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'held', now() + ($13::bigint * interval '1 microsecond'))
ON CONFLICT (request_id) DO NOTHING`

// sharedFingerprintSQL is the U6 PR2 owner-linkage check: do the contributor and
// requester workspaces share ANY captured card fingerprint (the lazy one-card
// operator funding both)? EXISTS is false when EITHER side has no captured
// fingerprint (default-ALLOW on missing — inconclusive never blocks honest
// cross-actor reuse). A plain read; no row lock.
const sharedFingerprintSQL = `SELECT EXISTS (
    SELECT 1 FROM workspace_card_fingerprints a
    JOIN workspace_card_fingerprints b ON a.fingerprint_hash = b.fingerprint_hash
    WHERE a.workspace_id = $1 AND b.workspace_id = $2)`

// capCountSQL is the 2.3b per-pair cap count, run INSIDE the mint tx AFTER
// CreditTx. The ordering is the whole trick: every mint for a given
// (requester, contributor) pair must take the SAME contributor-balance
// FOR UPDATE inside CreditTx, so concurrent serves of one pair serialize
// there — and under READ COMMITTED each statement snapshots fresh, so a
// count run after the lock was acquired always sees every prior committed
// mint for the pair. Exact under concurrency with ZERO new locks. The count
// includes the just-inserted claim row, so n > cap means "this would be the
// (cap+1)th". The window rides as microseconds (driver-proof interval).
// Access path: idx_pool_royalty_mints_requester (requester, created_at DESC).
const capCountSQL = `SELECT COUNT(*) FROM pool_royalty_mints
WHERE requester_workspace_id = $1 AND contributor_workspace_id = $2
  AND created_at > now() - ($3::bigint * interval '1 microsecond')`

// entryCountSQL is the per-ENTRY cap count (2.3b follow-up): mints for one
// entry_id across ALL contributors in the window. Like capCountSQL it has NO
// status filter — it counts held + final + REVOKED. That is DELIBERATE and
// the OPPOSITE of the detectors' exclude-revoked: a cap must count revoked so
// revoking a mint does NOT refund cap budget (else an abuser whose mints were
// revoked could immediately re-mint up to the cap again, reopening the exact
// exposure the revoke closed). Detection excludes revoked to avoid re-flagging
// an already-actioned party; a cap includes it to consume budget permanently
// on abuse. Hot-path: rides idx_pool_royalty_mints_entry (entry_id,created_at)
// from migration 0047 — never seq-scans.
const entryCountSQL = `SELECT COUNT(*) FROM pool_royalty_mints
WHERE entry_id = $1
  AND created_at > now() - ($2::bigint * interval '1 microsecond')`

// Minter mints s × avoided_COGS to the contributor, exactly once per serving
// request. Construct via NewMinter; the zero/nil Minter is inert.
type Minter struct {
	db      txBeginner
	ledger  ledgerCreditTx
	share   float64
	enabled func() bool

	// 2.3b per-pair cap: max mints per (requester, contributor) per rolling
	// window. 0 = disabled (the default) — the cap branch is skipped
	// entirely and mint behavior is byte-identical to pre-cap.
	capPerPair  int
	capPerEntry int
	capWindow   time.Duration

	// 2.3a holdback: new mints land status='held' with finalize_after =
	// now() + holdWindow; the finalize sweeper settles them. Defaults 72h.
	holdWindow time.Duration

	// U6 PR2: when true, deny a mint between two workspaces sharing a captured
	// card fingerprint (owner-linkage wash). false (default) skips the check —
	// existing tests and pre-flip behave exactly as before; production wires it on.
	linkageEnabled bool

	// anchor VALUES the gain (Proof-of-Improvement piece 1). Default CostAnchor{Share} ⇒ the mint
	// amount is byte-identical to today's `share × avoided_COGS`. Never touches the ledger.
	anchor Anchor
}

// SetAnchor overrides the gain-valuation anchor (reserved for the future held-benchmark caller).
// Unused on any live path this PR — main.go injects nothing; the cost default stands. nil is ignored.
func (m *Minter) SetAnchor(a Anchor) {
	if a != nil {
		m.anchor = a
	}
}

// SetOwnerLinkageCheck enables/disables the U6 PR2 owner-linkage wash guard.
// Off by default (the check is skipped — no workspace_card_fingerprints read);
// production enables it. Default-allow-on-missing still applies when enabled.
func (m *Minter) SetOwnerLinkageCheck(enabled bool) { m.linkageEnabled = enabled }

// SetHoldbackWindow configures the held->finalizable delay (Stage 2.3a).
// Non-positive values keep the 72h default. The TRIGGER that settles a held
// mint is decoupled from this ledger mechanism — the timed sweeper is the
// initial trigger; billing settlement can replace it without touching the
// mint path.
func (m *Minter) SetHoldbackWindow(d time.Duration) {
	if d > 0 {
		m.holdWindow = d
	}
}

// SetCap configures the per-pair rolling-window mint cap (2.3b primitive #1).
// perPair 0 disables (the default). The cap is what makes the bounded-
// exposure posture arithmetic: any party's worst case is
// cap × s × avoided_COGS per pair per window. Additive setter so existing
// construction sites stay unchanged.
func (m *Minter) SetCap(perPair int, window time.Duration) {
	if perPair < 0 {
		perPair = 0
	}
	m.capPerPair = perPair
	if window > 0 {
		m.capWindow = window
	}
}

// SetEntryCap configures the per-ENTRY rolling-window mint cap (2.3b
// follow-up). perEntry 0 disables (the default). Shares the same window as
// the per-pair cap when that window is set; the explicit window arg here keeps
// the setter self-contained for tests. Closes per-pair ≠ per-entry: one
// entry_id can accrue mints across contributors via ownership churn, which the
// per-pair cap does not bound.
func (m *Minter) SetEntryCap(perEntry int, window time.Duration) {
	if perEntry < 0 {
		perEntry = 0
	}
	m.capPerEntry = perEntry
	if window > 0 {
		m.capWindow = window
	}
}

// NewMinter builds a Minter. share is clamped to [0,1] so the Burn-and-Mint
// invariant (Talyvor nets (1−s) × avoided_COGS ≥ 0) cannot be violated by
// config. NaN is explicitly forced to 0 — every comparison on NaN is false,
// so without the IsNaN check a NaN share would sail through the clamp and
// poison the mint math (NaN × COGS = NaN, which also defeats amount <= 0).
// enabled is read per call so the flag stays live without rewiring.
func NewMinter(db txBeginner, ledger ledgerCreditTx, share float64, enabled func() bool) *Minter {
	if math.IsNaN(share) || share < 0 {
		share = 0
	}
	if share > 1 {
		share = 1
	}
	return &Minter{db: db, ledger: ledger, share: share, enabled: enabled, holdWindow: 72 * time.Hour, capWindow: 24 * time.Hour, anchor: CostAnchor{Share: share}}
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
	// NO HASH -> NO MINT (Stage 2.3.0, the privacy-coherence gate): a serve
	// whose evidence hashes could not be captured still serves and caches
	// normally, but writes no claim row and mints nothing — an unadjudicable
	// mint must never exist. WRITE-PATH ONLY: pre-2.3.0 rows with '' hashes
	// are historical data this gate never scans. Deflationary direction,
	// like every other defensive no-op here.
	if h.AnswerSHA256 == "" || h.PromptSHA256 == "" {
		return Result{}, nil
	}
	amount := m.anchor.Value(GainInput{AvoidedCOGSUSD: h.AvoidedCOGSUSD}) // default CostAnchor ⇒ share × avoided_COGS (byte-identical)
	// Non-finite amounts must NEVER reach the ledger: a NaN or ±Inf written
	// to lens_token_balances poisons the balance permanently (every later
	// bal+delta stays non-finite). amount <= 0 alone does NOT catch NaN.
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return Result{}, nil
	}

	tx, err := m.db.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("poolroyalty: begin mint tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// U6 PR2 owner-linkage: deny a pooled-royalty mint between two workspaces the
	// SAME operator controls (a shared captured card fingerprint — the lazy
	// one-card washer). Default-ALLOW on missing: the EXISTS is false when either
	// side has no captured fingerprint, so an inconclusive check NEVER blocks
	// honest cross-actor reuse (the rate cap bounds yield regardless). Read-only,
	// no row lock — runs BEFORE the claim and the credit's balance FOR UPDATE, so
	// there is no lock-ordering surface. A linked pair is a deflationary no-op,
	// like the same-id self-serve guard above.
	if m.linkageEnabled {
		var linked bool
		if err := tx.QueryRow(ctx, sharedFingerprintSQL, h.ContributorWorkspace, h.RequesterWorkspace).Scan(&linked); err != nil {
			return Result{}, fmt.Errorf("poolroyalty: owner-linkage check: %w", err)
		}
		if linked {
			return Result{}, nil
		}
	}

	tag, err := tx.Exec(ctx, insertClaimSQL,
		h.RequestID, h.RequesterWorkspace, h.ContributorWorkspace, h.Layer,
		h.EntryID, h.Provider, h.Model, h.Similarity, h.AvoidedCOGSUSD, amount,
		h.AnswerSHA256, h.PromptSHA256, m.holdWindow.Microseconds())
	if err != nil {
		return Result{}, fmt.Errorf("poolroyalty: insert mint claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Another serve (or a retry) already claimed this request_id — the
		// exactly-once suppression. Nothing was written; the deferred
		// rollback just ends the transaction.
		return Result{AlreadyMinted: true}, nil
	}

	// #145: the requester workspace id is DELIBERATELY omitted from the
	// contributor's ledger row (description + metadata) — tokens/history echoes
	// both verbatim, so naming the counterparty there is a cross-tenant identity
	// leak. The requester stays authoritatively in the admin-only
	// pool_royalty_mints claim row (inserted above), which is where the detector,
	// resolver, and per-pair cap read it. The contributor learns it was served,
	// never by whom.
	meta := map[string]interface{}{
		"request_id":       h.RequestID,
		"layer":            h.Layer,
		"entry_id":         h.EntryID,
		"avoided_cogs_usd": h.AvoidedCOGSUSD,
		"royalty_share":    m.share,
	}
	desc := fmt.Sprintf("pool royalty: %s pooled hit served", h.Layer)
	// Stage 2.3a: the mint credits HELD, not spendable — the LENS becomes
	// spendable (and supply-counted, via the TypePoolRoyalty row the sweeper
	// writes) only at finalize. Same position, same tx, same contributor-
	// balance FOR UPDATE as the old CreditTx — the cap COUNT below still
	// rides that lock, so its exactness proof is unchanged.
	if err := m.ledger.CreditHeldTx(ctx, tx, h.ContributorWorkspace, amount, TypePoolRoyaltyHeld, desc, meta); err != nil {
		return Result{}, fmt.Errorf("poolroyalty: credit contributor (held): %w", err)
	}
	// 2.3b per-pair cap — checked AFTER CreditTx on purpose: the credit just
	// acquired the contributor-balance FOR UPDATE, which serializes every
	// mint for this pair, making this count exact under concurrency (see
	// capCountSQL). Over the cap → return Capped and let the deferred
	// rollback discard the claim AND the credit atomically (the same path
	// AlreadyMinted uses). Skipped entirely when disabled (capPerPair == 0).
	if m.capPerPair > 0 {
		var n int64
		if err := tx.QueryRow(ctx, capCountSQL,
			h.RequesterWorkspace, h.ContributorWorkspace, m.capWindow.Microseconds()).Scan(&n); err != nil {
			return Result{}, fmt.Errorf("poolroyalty: cap count: %w", err)
		}
		if n > int64(m.capPerPair) {
			return Result{Capped: true, CapReason: "per_pair"}, nil
		}
	}
	// 2.3b per-ENTRY cap — a SECOND after-credit COUNT, keyed on entry_id
	// across all contributors. Checked AFTER per-pair (deterministic order:
	// if both would cap, per-pair wins and this query never runs). OPTION (a),
	// NO new lock: this is a lock-free SELECT; the tx still holds exactly ONE
	// lock (the contributor-balance FOR UPDATE in CreditHeldTx), so there is
	// no #32 lock-ordering surface. RESIDUAL (accepted): concurrent serves of
	// the same entry that STRADDLE an ownership-churn instant credit different
	// contributors (different balance rows, no shared lock) and may over-count
	// by the instantaneous concurrency — sub-cent, churn-only, economically
	// negligible. The common case (no churn mid-burst) reads one current owner
	// and serializes on that one balance row, so it is exact-for-free like
	// per-pair. A cap is an economic bound, not an accounting invariant.
	if m.capPerEntry > 0 {
		var n int64
		if err := tx.QueryRow(ctx, entryCountSQL,
			h.EntryID, m.capWindow.Microseconds()).Scan(&n); err != nil {
			return Result{}, fmt.Errorf("poolroyalty: entry cap count: %w", err)
		}
		if n > int64(m.capPerEntry) {
			return Result{Capped: true, CapReason: "per_entry"}, nil
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("poolroyalty: commit mint: %w", err)
	}
	// NOTE (2.3a): metrics.MintedTokens moved to the finalize sweeper — a
	// held mint hasn't entered circulation yet; the counter must agree with
	// the SQL supply stat, which counts the TypePoolRoyalty row written at
	// finalize.
	return Result{Minted: true, Amount: amount}, nil
}
