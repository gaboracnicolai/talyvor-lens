package attestation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/povi"
)

var errNonceMismatch = errors.New("attestation: wrap nonce != issued nonce")

// NodeInfo is the minimal per-node data the sweep needs, injected by main (backed by the node registry) so
// this package imports no mining/localrouter — keeping it mint-free + decoupled.
type NodeInfo struct {
	ID  string
	URL string
	Pub ed25519.PublicKey
}

// NodeSource lists the nodes to attest (active + registered pubkey).
type NodeSource interface {
	ActiveNodes(ctx context.Context) ([]NodeInfo, error)
}

// eatFetcher is the gateway→node call (Client satisfies it; tests stub it).
type eatFetcher interface {
	Fetch(ctx context.Context, nodeURL string, nonce int64) (povi.AttestationResponse, error)
}

// Sweep issues a fresh nonce per node, fetches the wrapped EAT, verifies (ed25519 wrap → NVIDIA EAT), and
// records the result. Every failure path records attestation_status='failed' (the nonce is consumed either
// way — single-use). INERT: constructed only when the capability flag is on (main gates it).
type Sweep struct {
	nodes    NodeSource
	fetch    eatFetcher
	verifier *Verifier
	store    *Store

	challengeWindow     time.Duration
	attestationValidity time.Duration
}

func NewSweep(nodes NodeSource, fetch eatFetcher, verifier *Verifier, store *Store) *Sweep {
	return &Sweep{
		nodes: nodes, fetch: fetch, verifier: verifier, store: store,
		challengeWindow: 2 * time.Minute, attestationValidity: 24 * time.Hour,
	}
}

// RunOnce attests every active node once. Per-node failures are logged + recorded, never fatal.
func (s *Sweep) RunOnce(ctx context.Context) (int, error) {
	if s == nil || s.nodes == nil || s.verifier == nil || s.store == nil || s.fetch == nil {
		return 0, nil // inert
	}
	nodes, err := s.nodes.ActiveNodes(ctx)
	if err != nil {
		return 0, err
	}
	verified := 0
	for _, n := range nodes {
		ok, err := s.verifyAndRecord(ctx, n)
		if err != nil {
			slog.Warn("attestation: node verify failed (recorded failed; retries next sweep)",
				slog.String("node", n.ID), slog.String("err", err.Error()))
		}
		if ok {
			verified++
		}
	}
	return verified, nil
}

// verifyAndRecord runs the full sequence for one node. Returns (true,nil) only on a recorded 'verified' row.
func (s *Sweep) verifyAndRecord(ctx context.Context, n NodeInfo) (bool, error) {
	if n.ID == "" || n.URL == "" || len(n.Pub) != ed25519.PublicKeySize {
		return false, nil
	}
	nonce := cryptoRandInt64()
	if err := s.store.IssueNonce(ctx, nonce, n.ID); err != nil {
		return false, err
	}
	fail := func(cause error) (bool, error) {
		_, _ = s.store.Consume(ctx, nonce, n.ID, ConsumeResult{Status: "failed"}, s.challengeWindow)
		return false, cause
	}

	resp, err := s.fetch.Fetch(ctx, n.URL, nonce)
	if err != nil {
		return fail(err)
	}
	// (i) node ed25519 wrap (reuse step a) — binds the response to this node's registered key.
	if err := povi.VerifyAttestation(resp, n.Pub); err != nil {
		return fail(err)
	}
	if resp.Nonce != nonce { // the wrap must be over the nonce we issued
		return fail(errNonceMismatch)
	}
	// (ii)-(iv) NVIDIA EAT: signature + x5c chain + claims + eat_nonce + key-binding.
	res, err := s.verifier.Verify(ctx, resp.EAT, nonce, n.Pub)
	if err != nil {
		return fail(err)
	}
	ok, err := s.store.Consume(ctx, nonce, n.ID, ConsumeResult{
		Status: "verified", GPUClass: res.GPUClass, CCMode: res.CCMode,
		EATDigest: res.EATDigest, KeyBound: res.KeyBound, ValidFor: s.attestationValidity,
	}, s.challengeWindow)
	return ok, err
}

// cryptoRandInt64 returns an unpredictable positive int64 (crypto/rand) for the single-use nonce.
func cryptoRandInt64() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.BigEndian.Uint64(b[:]) >> 1) // clear sign bit → positive
}
