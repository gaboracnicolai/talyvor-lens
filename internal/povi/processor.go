package povi

import (
	"context"
	"crypto/ed25519"

	"github.com/talyvor/lens/internal/metrics"
)

// PubKeyLookup returns the registered ed25519 public key for a node (the
// inference_nodes.ed25519_pubkey column), or an error if the node is unknown or
// has no pubkey on file. Injected so PoVI stays decoupled from the node
// registry / mining packages.
type PubKeyLookup func(ctx context.Context, nodeID string) (ed25519.PublicKey, error)

// StakeLookup reports whether a node is minting-eligible: it has an active
// stake at or above the minimum (Part 2). Injected so the gate stays decoupled.
type StakeLookup func(ctx context.Context, nodeID string) bool

// Processor is the Lens-side entry point for a receipt received alongside a
// network node's response: it verifies the signature, records the receipt for
// audit (verified or not), and — only when the receipt verifies AND provisional
// minting is explicitly enabled — performs a gated provisional mint.
//
// It runs on the network-node-served (compute-mining) accounting path, NOT the
// proxy request hot path, and has NO effect on non-network requests.
type Processor struct {
	store          *Store
	minter         Minter
	lookup         PubKeyLookup
	stakeEligible  StakeLookup
	mintingEnabled bool
}

// NewProcessor wires the audit store, the ledger minter, the node pubkey
// lookup, the stake-eligibility lookup (Part 2), and the provisional-minting
// flag (LENS_POVI_MINTING_ENABLED; default false). With minting disabled,
// Process verifies + records but never mints. A nil stakeEligible defaults to
// "not eligible" (safe): the mint gate then requires staking to be wired.
func NewProcessor(store *Store, minter Minter, lookup PubKeyLookup, stakeEligible StakeLookup, mintingEnabled bool) *Processor {
	if stakeEligible == nil {
		stakeEligible = func(context.Context, string) bool { return false }
	}
	return &Processor{store: store, minter: minter, lookup: lookup, stakeEligible: stakeEligible, mintingEnabled: mintingEnabled}
}

// MintingEnabled reports whether provisional (unsafe) minting is on.
func (p *Processor) MintingEnabled() bool { return p.mintingEnabled }

// ProcessResult is the outcome of handling one receipt.
type ProcessResult struct {
	Verified      bool    `json:"verified"`
	StakeEligible bool    `json:"stake_eligible"`
	Minted        bool    `json:"minted"`
	Amount        float64 `json:"amount"`
	Reason        string  `json:"reason,omitempty"` // why unverified/ineligible, when applicable
}

// Process verifies a receipt against the node's registered pubkey, records it
// (verified or not) for audit, and provisionally mints ONLY when verified AND
// minting is enabled. A forged/tampered receipt or an unknown node key yields
// Verified=false and never mints — that's the tamper-evidence this layer
// provides. (It does NOT detect a plausible-but-fabricated trace; that's
// Part 3's challenge job.)
func (p *Processor) Process(ctx context.Context, r Receipt) (ProcessResult, error) {
	verified := false
	reason := ""
	if pub, err := p.lookup(ctx, r.NodeID); err != nil {
		reason = "node public key not found: " + err.Error()
	} else if vErr := VerifyReceipt(r, pub); vErr != nil {
		reason = vErr.Error()
	} else {
		verified = true
	}

	metrics.POVIReceipt(verified)

	// firstRecord is the replay guard: RecordReceipt's ON CONFLICT result.
	// A receipt whose request_id was already recorded is a REPLAY — it is
	// still verified and reported, but it must never mint a second time
	// (the claim/RowsAffected shape shared with povi_challenges and
	// pool_royalty_mints). A nil store can't dedup and reports true.
	firstRecord := true
	if p.store != nil {
		inserted, err := p.store.RecordReceipt(ctx, r, verified)
		if err != nil {
			return ProcessResult{}, err
		}
		firstRecord = inserted
	}

	res := ProcessResult{Verified: verified, Reason: reason}
	if verified {
		// Part 2: minting also requires the node to be stake-eligible (an
		// active stake ≥ min). An unstaked/under-staked node's receipt is
		// recorded but ineligible to mint — even if minting is enabled.
		res.StakeEligible = p.stakeEligible(ctx, r.NodeID)
		switch {
		case !res.StakeEligible:
			if res.Reason == "" {
				res.Reason = "node not stake-eligible — receipt recorded but ineligible to mint"
			}
		case !firstRecord:
			res.Reason = "duplicate receipt (request_id already recorded) — replay, not minting"
		default:
			// MintFromReceipt is itself gated: it no-ops when mintingEnabled is
			// false, so this never mints by default.
			minted, amount, err := MintFromReceipt(ctx, p.minter, r, p.mintingEnabled)
			if err != nil {
				return res, err
			}
			res.Minted, res.Amount = minted, amount
		}
	}
	return res, nil
}
