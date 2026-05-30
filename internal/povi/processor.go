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
	mintingEnabled bool
}

// NewProcessor wires the audit store, the ledger minter, the node pubkey
// lookup, and the provisional-minting flag (LENS_POVI_MINTING_ENABLED; default
// false). With minting disabled, Process verifies + records but never mints.
func NewProcessor(store *Store, minter Minter, lookup PubKeyLookup, mintingEnabled bool) *Processor {
	return &Processor{store: store, minter: minter, lookup: lookup, mintingEnabled: mintingEnabled}
}

// MintingEnabled reports whether provisional (unsafe) minting is on.
func (p *Processor) MintingEnabled() bool { return p.mintingEnabled }

// ProcessResult is the outcome of handling one receipt.
type ProcessResult struct {
	Verified bool    `json:"verified"`
	Minted   bool    `json:"minted"`
	Amount   float64 `json:"amount"`
	Reason   string  `json:"reason,omitempty"` // why unverified, when applicable
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

	if p.store != nil {
		if err := p.store.RecordReceipt(ctx, r, verified); err != nil {
			return ProcessResult{}, err
		}
	}

	res := ProcessResult{Verified: verified, Reason: reason}
	if verified {
		// MintFromReceipt is itself gated: it no-ops when mintingEnabled is
		// false, so this never mints by default.
		minted, amount, err := MintFromReceipt(ctx, p.minter, r, p.mintingEnabled)
		if err != nil {
			return res, err
		}
		res.Minted, res.Amount = minted, amount
	}
	return res, nil
}
