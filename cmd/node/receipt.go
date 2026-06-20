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
// LEAF GRANULARITY — one Merkle leaf per OUTPUT RUNE of the response, NOT one
// leaf per model token. This is a DELIBERATE, CLOSED decision (audit item
// "per-token Merkle leaves"), not unfinished work or a temporary stand-in:
//
//   - Why runes: true per-token leaves need the model's actual output token
//     BOUNDARIES, and the local backends don't expose them. Every adapter
//     returns decoded TEXT plus an AGGREGATE token count only — Ollama's
//     eval_count (EvalCount), vLLM's completion_tokens (CompletionTokens),
//     llama.cpp's tokens_predicted — never token IDs, boundaries, or logprobs
//     (see providers.go InferResponse + the adapters). A count is not a
//     boundary sequence, so the per-token steps cannot be reconstructed from
//     what the node actually receives.
//   - Security is leaf-AGNOSTIC: the commitment's tamper-evidence and the
//     challenge-and-slash protocol (Part 3) prove trace retention + Merkle
//     consistency identically over runes or tokens. Per-token leaves buy
//     nothing cryptographically.
//   - Tokenizer re-tokenization was CONSIDERED AND REJECTED: re-tokenizing the
//     decoded text needn't reproduce the model's original generation
//     boundaries (a DIFFERENT approximation, not ground truth) and would ship
//     a heavy per-model-family tokenizer dependency into every node — cost for
//     no security gain.
//   - True per-token would require the backend to EMIT its token-id sequence.
//     If that ever lands, the already-per-step TraceBuilder feeds it with no
//     change to the Merkle structure. Until then, runes are the honest ceiling.
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

// sign builds, retains, and signs a receipt for one served request via the rune
// path: one leaf per output rune, tagged LeafKindRune. This is the live production
// path — local backends expose no token boundaries, so runes are the honest
// ceiling, and every receipt it emits is tagged 'rune' from day one.
func (rs *receiptSigner) sign(requestID, model string, inputTokens, outputTokens int, outputText string) povi.Receipt {
	return rs.signSteps(requestID, model, inputTokens, outputTokens, povi.StepsFromRunes(outputText), povi.LeafKindRune)
}

// signTokens is the true per-token path: one leaf per model token, tagged
// LeafKindToken. No provider emits token boundaries yet, so it is reached only by
// synthetic callers/tests today; if the provider interface grows token streaming,
// the server feeds the token sequence here with no change to the Merkle structure.
func (rs *receiptSigner) signTokens(requestID, model string, inputTokens, outputTokens int, tokens []string) povi.Receipt {
	return rs.signSteps(requestID, model, inputTokens, outputTokens, povi.StepsFromTokens(tokens), povi.LeafKindToken)
}

// signSteps is the shared core: it commits the (already-fed) leaf steps to a
// Merkle root, retains the trace for Part-3 challenges, and signs the receipt
// tagged with what its leaves represent. The tree machinery is leaf-agnostic — only
// the caller's feeding (runes vs tokens) and the kind tag differ.
func (rs *receiptSigner) signSteps(requestID, model string, inputTokens, outputTokens int, steps [][]byte, kind povi.LeafKind) povi.Receipt {
	tb := povi.NewTraceBuilder()
	for _, st := range steps {
		tb.AddStep(st)
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
		LeafCount:    tb.Len(),
		LeafKind:     kind,
	}
	return povi.SignReceipt(rs.priv, rec)
}
