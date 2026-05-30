package povi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/talyvor/lens/internal/metrics"
)

// Challenge-and-slash (PoVI Part 3) — the keystone that makes receipt-minting
// economically safe.
//
// SECURITY MODEL (read this): receipts (Part 1) are attestation, not proof of
// honest computation. Stake (Part 2) is slashable collateral with an unbonding
// delay so it stays slashable. Part 3 closes the loop: Lens RANDOMLY challenges
// a node to produce Merkle authentication paths for sampled positions in a
// receipt's committed trace. If the node can't produce valid paths (VerifyPath
// fails) or doesn't answer, its stake is SLASHED. To fake a receipt you'd have
// to answer random challenges about a trace you didn't honestly retain — and
// failing costs staked LENS.
//
// THIS IS PROBABILISTIC, NOT ABSOLUTE. Security is ECONOMIC: a rational node
// with stake at risk finds cheating unprofitable when
//
//	expected cost of cheating = P(challenge) × slash_amount  >  gain from cheating
//
// A low challenge rate means some bad receipts go unchallenged. The challenge
// rate and slash fraction are the deterrent knobs (higher rate = more overhead
// + stronger deterrent). A node willing to burn its stake can still slip one
// bad receipt through and be caught only probabilistically. We do NOT claim a
// cryptographic guarantee of honest computation.

// ── Lens-signed challenge (symmetric to Part 1: node signs receipts → Lens
// verifies; Lens signs challenges → node verifies). Auth narrowly protects the
// node's served-response content (trace leaves) from arbitrary callers + DoS;
// slashing integrity is self-verifying regardless of who asks. ──

// ChallengeRequest is what Lens sends a node: the positions to prove, signed by
// Lens so only the Lens key-holder can extract trace leaves.
type ChallengeRequest struct {
	RequestID string `json:"request_id"`
	Positions []int  `json:"positions"`
	Nonce     int64  `json:"nonce"`
	Signature []byte `json:"signature"`
}

// challengeCanonicalPayload is the deterministic, length-prefixed signed bytes:
// RequestID | Positions | Nonce (excludes Signature).
func challengeCanonicalPayload(requestID string, positions []int, nonce int64) []byte {
	var buf []byte
	var l [8]byte
	binary.BigEndian.PutUint32(l[:4], uint32(len(requestID)))
	buf = append(buf, l[:4]...)
	buf = append(buf, requestID...)
	binary.BigEndian.PutUint32(l[:4], uint32(len(positions)))
	buf = append(buf, l[:4]...)
	for _, p := range positions {
		binary.BigEndian.PutUint64(l[:], uint64(int64(p)))
		buf = append(buf, l[:]...)
	}
	binary.BigEndian.PutUint64(l[:], uint64(nonce))
	buf = append(buf, l[:]...)
	return buf
}

// SignChallenge produces a Lens-signed challenge.
func SignChallenge(lensPriv ed25519.PrivateKey, requestID string, positions []int, nonce int64) ChallengeRequest {
	req := ChallengeRequest{RequestID: requestID, Positions: positions, Nonce: nonce}
	req.Signature = ed25519.Sign(lensPriv, challengeCanonicalPayload(requestID, positions, nonce))
	return req
}

// VerifyChallenge checks the challenge's ed25519 signature against Lens's
// public key (the node calls this before answering).
func VerifyChallenge(req ChallengeRequest, lensPub ed25519.PublicKey) error {
	if len(lensPub) != ed25519.PublicKeySize {
		return errors.New("povi: invalid lens public key length")
	}
	if len(req.Signature) != ed25519.SignatureSize {
		return errors.New("povi: invalid challenge signature length")
	}
	if !ed25519.Verify(lensPub, challengeCanonicalPayload(req.RequestID, req.Positions, req.Nonce), req.Signature) {
		return errors.New("povi: challenge signature verification failed")
	}
	return nil
}

// ── challenge protocol types ──

// LeafProof is one position's answer: the trace leaf + its authentication path.
type LeafProof struct {
	Position int    `json:"position"`
	Leaf     []byte `json:"leaf"`
	Proof    Proof  `json:"proof"`
}

// PathProvider fetches sampled {leaf, proof} answers from the node that
// produced a receipt. The HTTP ChallengeClient is the production impl; tests
// use an in-memory provider backed by a real TraceCache. A non-nil error means
// no answer (timeout / unreachable) → treated as a failed challenge.
type PathProvider interface {
	FetchPaths(ctx context.Context, nodeID, nodeURL, requestID string, positions []int) ([]LeafProof, error)
}

// Slasher is the Part-2 slash trigger (*StakeManager satisfies it).
type Slasher interface {
	Slash(ctx context.Context, nodeID string, fraction float64, reason string) (float64, error)
}

// NodeURLLookup resolves a node's reachable URL (inference_nodes.url).
type NodeURLLookup func(ctx context.Context, nodeID string) (string, error)

// ChallengeResult is the outcome of a challenge.
type ChallengeResult string

const (
	ChallengePass    ChallengeResult = "pass"
	ChallengeFail    ChallengeResult = "fail"    // returned an invalid/wrong path
	ChallengeTimeout ChallengeResult = "timeout" // no response / unreachable
)

// Challenge is the audit record of one issued challenge.
type Challenge struct {
	ID            string          `json:"id"`
	RequestID     string          `json:"request_id"`
	NodeID        string          `json:"node_id"`
	WorkspaceID   string          `json:"workspace_id"`
	Positions     []int           `json:"positions"`
	Result        ChallengeResult `json:"result"`
	SlashedAmount float64         `json:"slashed_amount"`
	Reason        string          `json:"reason,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// challengeStore persists challenge records (pgChallengeStore is the real impl;
// tests use an in-memory fake). AlreadyChallenged is the double-slash guard.
type challengeStore interface {
	Record(ctx context.Context, c Challenge) error
	Get(ctx context.Context, id string) (*Challenge, error)
	AlreadyChallenged(ctx context.Context, requestID string) (bool, error)
	List(ctx context.Context, nodeID string) ([]Challenge, error)
}

var ErrAlreadyChallenged = errors.New("povi: receipt already challenged")

// ── the Challenger (keystone) ──

// Challenger issues random challenges against recorded receipts and slashes
// nodes that fail. Sampling is unpredictable to the node (crypto/rand), forcing
// it to retain the honest full trace.
type Challenger struct {
	nodeURL       NodeURLLookup
	provider      PathProvider
	slasher       Slasher
	store         challengeStore
	k             int     // positions sampled per challenge
	slashFraction float64 // fraction of stake slashed on failure
	now           func() time.Time
	idGen         func() string
}

// NewChallenger wires the node-URL lookup, the path provider (HTTP in prod),
// the slasher (StakeManager), the challenge store, and the deterrent knobs.
func NewChallenger(nodeURL NodeURLLookup, provider PathProvider, slasher Slasher, store challengeStore, positionsPerChallenge int, slashFraction float64) *Challenger {
	if positionsPerChallenge < 1 {
		positionsPerChallenge = 3
	}
	if slashFraction <= 0 || slashFraction > 1 {
		slashFraction = 0.5
	}
	return &Challenger{
		nodeURL:       nodeURL,
		provider:      provider,
		slasher:       slasher,
		store:         store,
		k:             positionsPerChallenge,
		slashFraction: slashFraction,
		now:           time.Now,
		idGen:         randomID,
	}
}

// SlashFraction / PositionsPerChallenge expose config for the status endpoint.
func (c *Challenger) SlashFraction() float64     { return c.slashFraction }
func (c *Challenger) PositionsPerChallenge() int { return c.k }

// Challenge issues one challenge against a recorded receipt: sample K positions,
// ask the node for the paths, verify each against the receipt's committed root,
// and slash on any failure / timeout. Double-slash-guarded per request.
func (c *Challenger) Challenge(ctx context.Context, rec StoredReceipt) (*Challenge, error) {
	done, err := c.store.AlreadyChallenged(ctx, rec.RequestID)
	if err != nil {
		return nil, err
	}
	if done {
		return nil, ErrAlreadyChallenged
	}
	if rec.LeafCount <= 0 {
		// Nothing committed to sample from — can't challenge meaningfully.
		return nil, fmt.Errorf("povi: receipt %q has no committed leaves", rec.RequestID)
	}

	positions, err := samplePositions(rec.LeafCount, c.k)
	if err != nil {
		return nil, err
	}
	root, err := decodeRootHex(rec.MerkleRootHex)
	if err != nil {
		return nil, err
	}

	ch := Challenge{
		ID: c.idGen(), RequestID: rec.RequestID, NodeID: rec.NodeID,
		WorkspaceID: rec.WorkspaceID, Positions: positions, CreatedAt: c.now().UTC(),
	}

	url, _ := c.nodeURL(ctx, rec.NodeID)
	answers, ferr := c.provider.FetchPaths(ctx, rec.NodeID, url, rec.RequestID, positions)
	switch {
	case ferr != nil:
		// No answer = treated as cheating (the node couldn't prove its work).
		ch.Result = ChallengeTimeout
		metrics.POVIChallengeTimeout()
	case !pathsValid(root, positions, rec.LeafCount, answers):
		ch.Result = ChallengeFail
	default:
		ch.Result = ChallengePass
	}

	if ch.Result != ChallengePass {
		ch.Reason = "challenge_" + string(ch.Result) + ":" + rec.RequestID
		slashed, serr := c.slasher.Slash(ctx, rec.NodeID, c.slashFraction, ch.Reason)
		if serr == nil {
			ch.SlashedAmount = slashed
			metrics.POVIChallengeSlash(slashed)
		}
	}

	metrics.POVIChallenge(string(ch.Result))
	if err := c.store.Record(ctx, ch); err != nil {
		return &ch, err
	}
	return &ch, nil
}

// pathsValid returns true only if EVERY sampled position has a leaf+proof that
// verifies against the committed root and is consistent with the leaf count.
func pathsValid(root [32]byte, positions []int, leafCount int, answers []LeafProof) bool {
	if len(answers) != len(positions) {
		return false
	}
	byPos := make(map[int]LeafProof, len(answers))
	for _, a := range answers {
		byPos[a.Position] = a
	}
	for _, p := range positions {
		a, ok := byPos[p]
		if !ok {
			return false
		}
		if a.Proof.LeafIndex != p || a.Proof.NumLeaves != leafCount {
			return false
		}
		if !VerifyPath(root, a.Leaf, a.Proof) {
			return false
		}
	}
	return true
}

// samplePositions picks min(k, n) DISTINCT positions in [0, n) using crypto/rand
// — unpredictable to the node (it can't know which positions will be asked, so
// it must retain the whole honest trace).
func samplePositions(n, k int) ([]int, error) {
	if n <= 0 {
		return nil, errors.New("povi: no positions to sample")
	}
	if k >= n {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
	chosen := make(map[int]struct{}, k)
	out := make([]int, 0, k)
	for len(out) < k {
		idx, err := randInt(n)
		if err != nil {
			return nil, err
		}
		if _, dup := chosen[idx]; dup {
			continue
		}
		chosen[idx] = struct{}{}
		out = append(out, idx)
	}
	return out, nil
}

// randInt returns a uniform random int in [0, n) from crypto/rand.
func randInt(n int) (int, error) {
	bn, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(bn.Int64()), nil
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "chal_" + fmt.Sprintf("%x", b[:])
}

// decodeRootHex parses the hex-encoded 32-byte Merkle root stored on a receipt.
func decodeRootHex(s string) ([32]byte, error) {
	var root [32]byte
	b, err := hex.DecodeString(s)
	if err != nil {
		return root, fmt.Errorf("povi: bad merkle root hex: %w", err)
	}
	if len(b) != 32 {
		return root, errors.New("povi: merkle root must be 32 bytes")
	}
	copy(root[:], b)
	return root, nil
}
