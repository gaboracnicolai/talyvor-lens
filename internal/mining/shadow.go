// shadow.go — shadow mode for mints that have not been validated.
//
// A shadow mint COMPUTES what it would pay, RECORDS that number, and CREDITS NOTHING. No balance
// changes, no spendable token appears, supply does not move. It exists so six unproven mints can
// run during a trial and be MEASURED, with testers told plainly they are not being paid for them.
//
// WHERE IT INTERCEPTS, AND WHY THERE. Every LENS mint funnels through exactly two kernels —
// applyTx (spendable) and heldInner (held) — which is the same property the U6 Sybil floor relies
// on: a new mint track cannot skip a check placed there. Shadow mode sits at those same two
// points, immediately after verifyEarn, so:
//
//   - a mint added next year is shadowable without touching its call site, and
//   - the interception happens BEFORE any ledger INSERT, so no row is written and then undone.
//
// ⚠ IT SITS AFTER verifyEarn ON PURPOSE, and this bounds what the data means: a workspace that
// is not verified-to-earn is rejected by the U6 floor before shadow mode is reached, so it
// produces NO shadow rows. The recorded numbers are therefore "what would have been paid to
// workspaces that could legitimately earn", not "what the mint would compute for anyone".
//
// ── HOW A SHADOW ROW IS STRUCTURALLY INCAPABLE OF BECOMING REAL ─────────────
//
// Not a flag. Four independent obstacles, three of them compile-time or schema-level:
//
//  1. THE SINK CANNOT REACH THE LEDGER. ShadowSink.RecordShadow takes NO pgx.Tx — so it cannot
//     join the mint transaction — and is handed no LedgerStore, so it has nothing to credit
//     with. Widening that interface is the only way in, and TestShadowSink_CannotReachTheLedger
//     reads the source to fail if anyone does.
//  2. A DIFFERENT TABLE. Shadow rows go to lens_shadow_mints. Every balance, ledger sum and
//     supply figure in this repository queries lens_token_ledger, so none of them can see a
//     shadow row. That converts an N-reader obligation ("remember to exclude shadow rows") into
//     a ONE-WRITER obligation ("never write one into the ledger"), which
//     TestShadowMint_NeverTouchesTheTokenLedger pins by reading this file's code.
//  3. NO CREDIT PATH READS IT. There is no settle, finalize, or promote step from
//     lens_shadow_mints into the ledger. Making a shadow row real requires WRITING NEW CODE that
//     inserts into the ledger from this table — a reviewable change, not a config flip.
//  4. FAIL-CLOSED ON RECORD FAILURE. If the observation cannot be written, the mint is still
//     refused. Falling through to a real credit would pay for an unproven mint at exactly the
//     moment we lost the ability to see it.
package mining

import (
	"context"
	"errors"
	"log/slog"
)

// ErrShadowedMint is returned in place of a credit when a mint type is in shadow mode. The mint
// tx rolls back — no row, no balance change — exactly like ErrEarnNotVerified, and every mint
// call site already treats a ledger error as non-fatal-to-serve. So a shadowed mint simply does
// not pay, and the request it rode on is unaffected.
var ErrShadowedMint = errors.New("mining: mint is in SHADOW MODE — recorded as a hypothetical, credited nothing")

// ShadowSink persists what a mint would have paid.
//
// ⚠ THE SIGNATURE IS THE SAFETY PROPERTY. No pgx.Tx: the sink cannot write inside the mint
// transaction, so it cannot reach lens_token_ledger through it. No store or ledger parameter: it
// has nothing to credit with. Do not add either — a sink that can credit is one refactor away
// from a shadow row becoming real, and that is precisely what this design exists to prevent.
type ShadowSink interface {
	RecordShadow(ctx context.Context, workspaceID, mintType string, wouldMintMicroLENS int64, metadata map[string]any) error
}

// ShadowableMintTypes returns the ledger txTypes of the six mints whose measurement has never
// been validated: POVI, annotation, eval-contribution, routing-prediction, latency-locality and
// confidential-compute. These are the ones worth running in shadow.
//
// Deliberately a FUNCTION returning a fresh slice, not an exported var: a caller cannot mutate
// the set out from under the gate.
func ShadowableMintTypes() []string {
	return []string{
		"receipt_mine_provisional",  // POVIMintingEnabled
		TypeAnnotationMine,          // AnnotationMintingEnabled
		TypeEvalContributionHeld,    // EvalContributionMintingEnabled
		TypeRoutingPredictionHeld,   // RoutingPredictionMintingEnabled
		TypeLatencyLocalityHeld,     // LatencyMintingEnabled
		TypeConfidentialComputeHeld, // ConfidentialMintingEnabled
	}
}

// SetShadowSink puts the named mint types into shadow mode. Call once at startup.
//
// sink==nil or an empty type set ⇒ shadow mode is entirely off and every mint credits exactly as
// before (the byte-identical default; every existing test exercises this path).
func (s *LedgerStore) SetShadowSink(sink ShadowSink, types []string) {
	s.shadowSink = sink
	s.shadowTypes = make(map[string]struct{}, len(types))
	for _, t := range types {
		s.shadowTypes[t] = struct{}{}
	}
}

// shadowIntercept is the gate, called from applyTx and heldInner immediately after verifyEarn.
//
// Returns nil for a type that is not shadowed — the caller proceeds to credit normally. Returns
// ErrShadowedMint for a shadowed type, after recording the hypothetical, so the caller's tx rolls
// back without writing a ledger row.
func (s *LedgerStore) shadowIntercept(
	ctx context.Context,
	workspaceID, txType string,
	amount int64,
	metadata map[string]any,
) error {
	if s.shadowSink == nil || len(s.shadowTypes) == 0 {
		return nil
	}
	if _, shadowed := s.shadowTypes[txType]; !shadowed {
		return nil
	}
	// Recorded OUTSIDE the caller's transaction, by construction: the sink has no tx to write
	// through. That is deliberate on both counts — it keeps the sink unable to touch the ledger,
	// and it means the observation survives the rollback that ErrShadowedMint causes. An
	// observation is not a financial fact and must not vanish with the mint it describes.
	if err := s.shadowSink.RecordShadow(ctx, workspaceID, txType, amount, metadata); err != nil {
		// FAIL CLOSED: still refuse the credit. The alternative pays for an unproven mint at the
		// exact moment we stopped being able to measure it.
		slog.Warn("shadow mint: record failed; credit still refused",
			slog.String("workspace_id", workspaceID),
			slog.String("mint_type", txType),
			slog.String("err", err.Error()),
		)
		return ErrShadowedMint
	}
	slog.Debug("shadow mint recorded; nothing credited",
		slog.String("workspace_id", workspaceID),
		slog.String("mint_type", txType),
		slog.Int64("would_mint_micro_lens", amount),
	)
	return ErrShadowedMint
}
