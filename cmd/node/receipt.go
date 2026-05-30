package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"time"

	"github.com/talyvor/lens/internal/povi"
)

// receiptSigner produces a signed PoVI receipt for each served request: it
// builds a Merkle commitment over the generation trace, signs the receipt with
// the node's ed25519 private key, and retains the trace so a later challenge
// (Part 3) can be answered with sampled authentication paths.
//
// LEAF GRANULARITY (documented stand-in): one Merkle leaf per OUTPUT RUNE of
// the response — NOT one leaf per model token. The commitment is real and
// tamper-evident, but it commits to the response's runes, not the model's
// internal token steps, until per-token streaming is wired into the provider
// interface. The TraceBuilder is already per-step, so switching to true
// per-token leaves later changes only the feeding here, not the structure.
type receiptSigner struct {
	priv        ed25519.PrivateKey
	nodeID      string
	workspaceID string
	traces      *povi.TraceCache
	now         func() time.Time
}

// newReceiptSigner builds a signer from the node's persisted state. Returns
// (nil, nil) when the node has no signing key (older nodes registered before
// PoVI) — the caller treats a nil signer as "produce no receipt".
func newReceiptSigner(state NodeState) (*receiptSigner, error) {
	if state.Ed25519Priv == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(state.Ed25519Priv)
	if err != nil {
		return nil, err
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, errors.New("node: stored ed25519 private key has wrong length")
	}
	return &receiptSigner{
		priv:        ed25519.PrivateKey(raw),
		nodeID:      state.NodeID,
		workspaceID: state.WorkspaceID,
		traces:      povi.NewTraceCache(povi.DefaultTraceTTL),
		now:         time.Now,
	}, nil
}

// sign builds, retains, and signs a receipt for one served request.
func (rs *receiptSigner) sign(requestID, model string, inputTokens, outputTokens int, outputText string) povi.Receipt {
	tb := povi.NewTraceBuilder()
	for _, r := range outputText {
		tb.AddStep([]byte(string(r))) // one leaf per output rune (stand-in)
	}
	// Retain the trace so Part 3 can produce sampled authentication paths.
	rs.traces.Put(requestID, tb.Steps())

	rec := povi.Receipt{
		RequestID:    requestID,
		NodeID:       rs.nodeID,
		WorkspaceID:  rs.workspaceID,
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		MerkleRoot:   tb.Root(),
		Timestamp:    rs.now().Unix(),
	}
	return povi.SignReceipt(rs.priv, rec)
}
