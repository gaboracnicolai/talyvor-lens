// Package povi implements the Proof-of-Verifiable-Inference receipt layer
// (Token Economy Phase 1, Part 1).
//
// WHAT THIS IS: a per-request, node-signed receipt that commits to the request
// metadata and a Merkle root over the generation trace. It provides ATTESTATION
// (a node vouches, under its key, for what it served) and TAMPER-EVIDENCE (any
// later mutation of a signed field or the committed trace is detectable).
//
// WHAT THIS IS NOT — read carefully: this is NOT proof of honest computation. A
// node can sign a receipt over a FABRICATED response, or commit to a self-
// consistent FAKE trace, and this layer will happily verify it — because the
// signature only proves the holder of the node's private key produced the
// bytes, and the Merkle root only proves the trace wasn't altered AFTER
// commitment. Catching a plausible-but-fabricated trace requires random
// challenge-and-slash backed by stake — that is Part 3's job, not this layer's.
// Describe a receipt as "attestation + tamper-evidence", never as "proof of
// honest computation", in code, docs, and API responses.
//
// MINTING SAFETY: minting LENS from a receipt is therefore UNSAFE on receipt-
// alone. MintFromReceipt (mint.go) is gated OFF by default and emits a loud
// provisional/unsafe warning if enabled. Default behavior: verify + record for
// audit, mint NOTHING.
//
// EXISTING TRUST-BASED MINT (the thing PoVI replaces): there is already a
// trust-based minting path — ComputeMiner.RecordServedRequest mints LENS on
// every node-served request WITHOUT a receipt. PoVI is designed to replace it.
// That path remains active and UNSECURED until Part 3 (challenge-and-slash)
// provides the economic security to switch minting onto verified receipts and
// retire the blind credit. This is a known, tracked item — the explicit target
// of Parts 2/3, not an oversight.
package povi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
)

// Receipt is a node-signed attestation for one served inference request. The
// Signature is ed25519 over CanonicalPayload (which excludes the Signature).
type Receipt struct {
	RequestID    string   `json:"request_id"`
	NodeID       string   `json:"node_id"`
	WorkspaceID  string   `json:"workspace_id"`
	Model        string   `json:"model"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	MerkleRoot   [32]byte `json:"merkle_root"`
	Timestamp    int64    `json:"timestamp"`
	Signature    []byte   `json:"signature"`
	// LeafCount is the number of leaves in the committed trace (output runes).
	// It is NOT part of the signed CanonicalPayload — it's an informational
	// hint so Lens knows the sampling range [0, LeafCount) for a Part-3
	// challenge. A node can't benefit from lying about it: the sampled
	// positions must still verify against the SIGNED MerkleRoot (understating
	// just narrows the range checked; overstating gets it asked for positions
	// it can't answer → the challenge fails).
	LeafCount int `json:"leaf_count,omitempty"`
	// LeafKind records what each committed leaf REPRESENTS — output runes (the
	// stand-in for backends with no token boundaries) or true model tokens. Like
	// LeafCount it is NOT part of the signed CanonicalPayload, so rune-rooted
	// receipts signed before this field existed stay verifiable unchanged; it labels
	// granularity for the Part-3 challenge + audit. A node can't benefit from
	// mislabeling: sampled positions must still verify against the SIGNED MerkleRoot,
	// and the leaves it returns to answer are self-evidently runes or tokens. Empty
	// is treated as "rune" by the store (the historical default).
	LeafKind LeafKind `json:"leaf_kind,omitempty"`
}

// GenerateNodeKey creates a fresh ed25519 keypair for a node. The node keeps
// the private key (state file) and registers the public key with Lens.
func GenerateNodeKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// CanonicalPayload is the deterministic, length-prefixed byte layout that the
// signature covers: RequestID|NodeID|WorkspaceID|Model|InputTokens|
// OutputTokens|MerkleRoot|Timestamp. Variable-length fields are length-prefixed
// (4-byte big-endian) so adjacent fields can never blur into one another
// (the "ab|c" vs "a|bc" ambiguity), making the signed message unambiguous.
func CanonicalPayload(r Receipt) []byte {
	var buf []byte
	putStr := func(s string) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(s)))
		buf = append(buf, l[:]...)
		buf = append(buf, s...)
	}
	putI64 := func(v int64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v))
		buf = append(buf, b[:]...)
	}
	putStr(r.RequestID)
	putStr(r.NodeID)
	putStr(r.WorkspaceID)
	putStr(r.Model)
	putI64(int64(r.InputTokens))
	putI64(int64(r.OutputTokens))
	buf = append(buf, r.MerkleRoot[:]...)
	putI64(r.Timestamp)
	return buf
}

// SignReceipt signs the receipt's canonical payload with the node's private
// key and returns the receipt with Signature populated.
func SignReceipt(priv ed25519.PrivateKey, r Receipt) Receipt {
	r.Signature = ed25519.Sign(priv, CanonicalPayload(r))
	return r
}

// VerifyReceipt checks the ed25519 signature against the node's registered
// public key. A nil error means the receipt is authentic AND untampered — i.e.
// attested + tamper-evident. It does NOT mean the underlying computation was
// honest (see the package doc).
func VerifyReceipt(r Receipt, pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("povi: invalid public key length")
	}
	if len(r.Signature) != ed25519.SignatureSize {
		return errors.New("povi: invalid signature length")
	}
	if !ed25519.Verify(pub, CanonicalPayload(r), r.Signature) {
		return errors.New("povi: signature verification failed")
	}
	return nil
}

// EncodePublicKey / DecodePublicKey marshal a node public key for storage in
// the node registry (the inference_nodes.ed25519_pubkey column) and transport
// at registration.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, errors.New("povi: decoded public key has wrong length")
	}
	return ed25519.PublicKey(b), nil
}
