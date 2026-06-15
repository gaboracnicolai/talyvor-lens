// mint_gate.go — the U6 Sybil floor at the ledger chokepoint.
//
// Every LENS mint funnels through exactly two kernels: applyTx (spendable
// Credit/CreditTx) and heldInner (CreditHeldTx). The verified-to-earn gate is
// placed in BOTH, discriminated by the txType against mintTypes, so it covers
// all seven mint paths at once and a new mint track CANNOT skip it — while
// conservation credits (marketplace_*, *unstake, transfer) and the held
// finalize/revoke pass through untouched.
package mining

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/metrics"
)

// ErrEarnNotVerified is returned by a mint-type credit when the earning
// workspace is not verified-to-earn. The mint tx rolls back — no ledger row, no
// balance change, no metrics. Mint call sites already treat a ledger error as
// non-fatal-to-serve, so a blocked mint simply doesn't pay.
var ErrEarnNotVerified = errors.New("mining: workspace not verified to earn (U6 Sybil floor)")

// ErrNoMintRequestID is returned by CreditOnce when requestID is empty —
// fail-closed: an idempotent mint REQUIRES a server-derived work-product key. A
// caller-supplied or absent id is rejected rather than minting unprotected.
var ErrNoMintRequestID = errors.New("mining: idempotent mint requires a server-derived request_id")

// MintVerifier decides whether a workspace may EARN (mint / accrue royalty).
// Injected via LedgerStore.SetMintVerifier; nil ⇒ no gate (every existing test
// and the pre-flip default behave exactly as before). MayEarn runs on the MINT
// tx so it reads consistently with the credit it gates.
type MintVerifier interface {
	MayEarn(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error)
}

// SetMintVerifier wires the verified-to-earn gate. Call once at startup. nil is
// a no-op (allow all). Production wires a real verifier unconditionally (a
// safety restriction must not be liftable by the economy toggle); tests leave
// it nil.
func (s *LedgerStore) SetMintVerifier(v MintVerifier) { s.verifier = v }

// mintTypes is the set of ledger txTypes that represent the MINT MOMENT — where
// new value first accrues to the earner — and therefore require a verified
// workspace.
//
// ⚠️ DELIBERATELY NOT EQUAL to GetTotalSupply's allow-list. The two sets answer
// DIFFERENT questions; a "consistency fix" aligning them would silently open a
// Sybil hole. Pin the REASONING, not just the values:
//
//   - We gate at the mint MOMENT; supply COUNTS at a (sometimes later) moment.
//   - INCLUDES TypePoolRoyaltyHeld — the held credit IS the pool-royalty mint
//     moment (the worst Sybil hole). Supply counts it LATER, as TypePoolRoyalty
//     at finalize. Dropping it here "to match supply" would UN-GATE the held
//     mint: an unverified workspace could accrue held royalty. MUST stay.
//   - INCLUDES "receipt_mine_provisional" — a real PoVI mint moment, even though
//     it is supply-EXCLUDED (its own go-live treatment). Gating it is correct.
//     (Literal, not povi.TypeReceiptMineProvisional, to avoid an import cycle —
//     mining is imported BY povi. TestMintTypes_CoversPoVIReceipt in cmd/lens
//     pins the literal to that constant.)
//   - EXCLUDES TypePoolRoyalty — the finalize SETTLEMENT moves already-gated
//     held value to spendable; it is not a new mint. Gating it would DOUBLE-GATE
//     finalize (a workspace verified at mint-time but later un-verified could
//     never settle value it legitimately earned). MUST stay excluded.
//   - EXCLUDES conservation (marketplace_*, *unstake, transfer) and burns
//     (pool_royalty_revoked) — they move or destroy existing value, never mint.
//
// mintTypeList is the SINGLE SOURCE OF TRUTH for the mint-moment type set. BOTH
// the verified-to-earn gate (IsMintType, via the derived map) AND the PR2 rate
// cap (the SUM's `type = ANY($mintTypeList)`) read it — never a second copy, so
// they cannot diverge (TestMintTypeList_IsSingleSource pins this).
var mintTypeList = []string{
	TypeCacheMine,
	TypeComputeMine,
	TypeEmbeddingMine,
	TypeAnnotationMine,
	TypePatternMine,
	"receipt_mine_provisional", // == povi.TypeReceiptMineProvisional (cycle-free literal)
	TypePoolRoyaltyHeld,
}

// mintTypes is DERIVED from mintTypeList (not a second literal) for O(1) lookup.
var mintTypes = func() map[string]struct{} {
	m := make(map[string]struct{}, len(mintTypeList))
	for _, t := range mintTypeList {
		m[t] = struct{}{}
	}
	return m
}()

// IsMintType reports whether txType is a mint-moment type that the verified-to-
// earn gate covers. Exported so a cross-package test (cmd/lens) can pin the set
// against every recon mint path, including povi.TypeReceiptMineProvisional.
func IsMintType(txType string) bool {
	_, ok := mintTypes[txType]
	return ok
}

// verifyEarn enforces the gate for a mint-type credit inside the caller's tx.
// No-op when no verifier is wired or txType is not a mint type (conservation /
// finalize / burn pass through). Returns ErrEarnNotVerified to roll the mint back.
func (s *LedgerStore) verifyEarn(ctx context.Context, tx pgx.Tx, workspaceID, txType string) error {
	if s.verifier == nil || !IsMintType(txType) {
		return nil
	}
	ok, err := s.verifier.MayEarn(ctx, tx, workspaceID)
	if err != nil {
		return fmt.Errorf("mining: earn verification for %q: %w", workspaceID, err)
	}
	if !ok {
		return ErrEarnNotVerified
	}
	return nil
}

// ─── U6 PR2: per-identity mint rate cap ───────────────────────────────────

// ErrMintRateCapExceeded is returned by a mint when the workspace would exceed
// its rolling-window minted-LENS ceiling. The mint tx rolls back (no ledger
// row, no balance change, no metrics) — same shape as ErrEarnNotVerified.
var ErrMintRateCapExceeded = errors.New("mining: workspace mint rate cap exceeded (U6 PR2)")

// SetMintRateCap wires the per-workspace rolling-window mint ceiling (LENS
// minted per window). capLENS <= 0 disables it (no cap). window <= 0 falls back
// to 24h. Call once at startup, UNCONDITIONALLY — a safety restriction the
// economy toggle must not lift (mirrors SetMintVerifier).
func (s *LedgerStore) SetMintRateCap(capLENS float64, window time.Duration) {
	s.mintRateCap = capLENS
	if window <= 0 {
		window = 24 * time.Hour
	}
	s.mintRateWindow = window
}

// mintRateSumSQL sums the LENS a workspace has MINTED within the rolling window.
// type = ANY($3) is fed mintTypeList VERBATIM (the floor's single source) — so
// it includes pool_royalty_held (the mint moment) and excludes the pool_royalty
// finalize-settlement, never double-counting a held mint that later finalizes.
// `amount > 0` drops the revoke/burn rows. Index-only over idx_ledger_mint_rate
// (workspace_id, created_at) INCLUDE (type, amount) — migration 0058.
const mintRateSumSQL = `SELECT COALESCE(SUM(amount), 0)
FROM lens_token_ledger
WHERE workspace_id = $1
  AND created_at  > now() - ($2::bigint * interval '1 microsecond')
  AND amount      > 0
  AND type        = ANY($3)`

// checkMintRateCap enforces the rolling-window ceiling for a mint-type credit,
// inside the caller's mint tx. It MUST be called AFTER the lens_token_balances
// FOR UPDATE the credit takes, so concurrent same-workspace mints serialize and
// the SUM sees prior committed mints (exact, no race). No-op when the cap is
// disabled or txType is not a mint type (conservation / finalize / burn pass).
func (s *LedgerStore) checkMintRateCap(ctx context.Context, tx pgx.Tx, workspaceID, txType string, amount float64) error {
	if s.mintRateCap <= 0 || !IsMintType(txType) {
		return nil
	}
	window := s.mintRateWindow
	if window <= 0 {
		window = 24 * time.Hour
	}
	var minted float64
	if err := tx.QueryRow(ctx, mintRateSumSQL, workspaceID, window.Microseconds(), mintTypeList).Scan(&minted); err != nil {
		return fmt.Errorf("mining: mint rate-cap sum for %q: %w", workspaceID, err)
	}
	if minted+amount > s.mintRateCap {
		return ErrMintRateCapExceeded
	}
	return nil
}

const insertMintClaimSQL = `
	INSERT INTO mint_idempotency (request_id, workspace_id, mint_type, amount)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (request_id, workspace_id, mint_type) DO NOTHING`

// CreditOnce is an idempotent mint: it credits like Credit but is replay-safe on
// (requestID, workspaceID, txType). A repeat is a no-op (alreadyMinted=true,
// nothing credited). An EMPTY requestID returns ErrNoMintRequestID and mints
// nothing (fail-closed — idempotency requires a server-derived work-product
// key). Claim, verified-gate, and credit are ONE tx, so a duplicate or a
// gate-blocked mint leaves no partial state and a blocked mint does NOT consume
// the idempotency key (the claim rolls back with it).
//
// Used by the compute / cache / embedding tracks, whose request_id MUST be
// server-derived by the (future) live wire-up — see each RecordServed* caller.
func (s *LedgerStore) CreditOnce(ctx context.Context, requestID, workspaceID string, amount float64, txType, description string, metadata map[string]interface{}) (alreadyMinted bool, err error) {
	if amount <= 0 {
		return false, errors.New("mining: credit amount must be positive")
	}
	if requestID == "" {
		return false, ErrNoMintRequestID
	}
	if s.pool == nil {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("mining: begin credit-once tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, insertMintClaimSQL, requestID, workspaceID, txType, amount)
	if err != nil {
		return false, fmt.Errorf("mining: insert mint claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return true, nil // already claimed — exactly-once suppression, nothing minted
	}
	// applyTx runs the verified-to-earn gate (mint type → MayEarn) + the credit.
	// A gate block (ErrEarnNotVerified) rolls back the claim too.
	if err := s.applyTx(ctx, tx, workspaceID, amount, txType, description, metadata, true); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("mining: commit credit-once: %w", err)
	}
	metrics.MintedTokens(amount)
	return false, nil
}
