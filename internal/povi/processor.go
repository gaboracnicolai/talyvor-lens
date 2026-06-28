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
	// isProbe (P1 #10, optional) is the proof-of-benchmark probe-mint SUPPRESSION: a point existence
	// check on benchmark_probes.request_id. nil ⇒ no suppression (byte-identical). Wired only when
	// LENS_PROOF_OF_BENCHMARK_ENABLED is on. SUPPRESSION-ONLY: it can return "this is a probe → don't
	// mint", never cause a mint.
	isProbe func(ctx context.Context, requestID string) (bool, error)
}

// SetProbeChecker wires the proof-of-benchmark probe-mint suppression (P1 #10). nil ⇒ no suppression
// (the mint path is byte-identical). A setter so NewProcessor's signature stays put.
//
// HONEST-NODE guarantee + documented residual: this records-but-skips the mint for a receipt whose
// request_id is a verifier-induced probe. A MALICIOUS node can BYPASS it by signing a non-probe
// request_id for a probe response — but that is the SAME pre-existing receipt-fabrication capability
// (the receipt request_id is node-asserted; a node can already mint receipts for fabricated work,
// deterred by challenge-and-slash + stake + the 24h rate-cap + the reputation bond). Probes add NO new
// surface: the node is BLIND (cannot distinguish a probe from real traffic), so it cannot selectively
// target probes; any evasion is bounded by the probe rate and dominated by the pre-existing
// fabrication path. The gateway-bound-request_id fix is the tracked pre-public-mint gate, NOT #10.
func (p *Processor) SetProbeChecker(fn func(ctx context.Context, requestID string) (bool, error)) {
	p.isProbe = fn
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

	// P1 #10 probe-mint SUPPRESSION (point lookup on benchmark_probes.request_id; nil checker ⇒
	// skipped, byte-identical). A verifier-induced probe receipt is RECORDED above (audit) but must
	// NOT mint. Suppression-only — see SetProbeChecker for the honest-node guarantee + residual.
	probe := false
	if verified && p.isProbe != nil {
		ip, err := p.isProbe(ctx, r.RequestID)
		if err != nil {
			return ProcessResult{}, err
		}
		probe = ip
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
		case probe:
			// Verifier-induced proof-of-benchmark probe: recorded for audit, never minted.
			res.Reason = "probe receipt (verifier-induced measurement) — recorded, not minted"
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
