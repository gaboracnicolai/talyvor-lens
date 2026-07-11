package povi

import (
	"context"
	"log/slog"

	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
)

// Minter is the minimal ledger-credit surface MintFromReceipt needs. The real
// *mining.LedgerStore satisfies this signature exactly, so PoVI stays decoupled
// from the ledger package (and tests pass a fake minter). amount is µLENS (SEC-2).
type Minter interface {
	Credit(ctx context.Context, workspaceID string, amount int64, txType, description string, metadata map[string]interface{}) error
}

// TypeReceiptMineProvisional tags a provisional, receipt-based LENS mint in the
// ledger. The "_provisional" suffix is deliberate: this credit path is UNSAFE
// without stake + challenge (Parts 2/3) and is gated off by default.
const TypeReceiptMineProvisional = "receipt_mine_provisional"

// ReceiptMineRate is the provisional per-1k-output-token rate in µLENS (SEC-2),
// mirroring the existing ComputeMineBaseRate. It only matters if the unsafe
// minting flag is enabled (default off), so it is academic in normal operation.
const ReceiptMineRate int64 = 50_000 // 0.050 LENS

// ProvisionalMintAmount is the µLENS a receipt WOULD mint (rate × output tokens
// / 1000). Exposed so the /v1/povi/status endpoint can show "would mint X"
// without actually minting. A mint rounds DOWN (MulFloor) — SEC-2.
func ProvisionalMintAmount(r Receipt) int64 {
	return mining.MulFloor(ReceiptMineRate, float64(r.OutputTokens)/1000.0)
}

// MintFromReceipt is the GATED, provisional receipt→LENS mint.
//
// SAFETY: minting from a receipt is UNSAFE on receipt-alone — a node can sign a
// receipt over a fabricated trace and this layer can't tell (that's Part 3's
// challenge-and-slash job). So:
//
//   - enabled == false (THE DEFAULT): mint NOTHING. No ledger write, no metric.
//     Returns minted=false. This is the safe path; callers still verify +
//     record the receipt for audit, they just don't credit LENS.
//   - enabled == true (test-only / explicitly opted in via
//     LENS_POVI_MINTING_ENABLED): log a LOUD provisional/unsafe warning, credit
//     LENS via the ledger, and bump the provisional-mint metric so flipping the
//     flag is observable.
//
// This function NEVER asserts the receipt proves honest computation — it is
// attestation + tamper-evidence only.
func MintFromReceipt(ctx context.Context, minter Minter, r Receipt, enabled bool) (minted bool, amount int64, err error) {
	if !enabled {
		// Default, safe behavior: do not mint on receipt-alone.
		return false, 0, nil
	}
	amount = ProvisionalMintAmount(r)
	slog.Warn("povi: PROVISIONAL minting is ENABLED — this is UNSAFE without stake + challenge (Parts 2/3); "+
		"a node can mint LENS against a FABRICATED trace because a receipt is attestation + tamper-evidence, "+
		"NOT proof of honest computation",
		slog.String("node_id", r.NodeID),
		slog.String("workspace_id", r.WorkspaceID),
		slog.Int64("amount_ulens", amount),
		slog.String("tx_type", TypeReceiptMineProvisional))
	metrics.POVIProvisionalMint()
	if err := minter.Credit(ctx, r.WorkspaceID, amount, TypeReceiptMineProvisional,
		"provisional receipt mint (UNSAFE — no stake/challenge; Parts 2/3 pending)",
		map[string]interface{}{
			"node_id":       r.NodeID,
			"request_id":    r.RequestID,
			"output_tokens": r.OutputTokens,
			"provisional":   true,
		}); err != nil {
		return false, amount, err
	}
	return true, amount, nil
}
