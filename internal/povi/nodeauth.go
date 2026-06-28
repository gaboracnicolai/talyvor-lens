package povi

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"
)

// NodeAuthToken is a short-lived, request-bound credential the GATEWAY signs (with its EXISTING
// challenge private key) to authenticate an auto-routed POST /inference to a registered node. It is
// the request-auth twin of the challenge (symmetric to Part 1/3: node signs receipts → Lens
// verifies; Lens signs challenges/tokens → node verifies with the SAME pinned challenge pubkey).
//
// Binding {node_id, request_id, body_sha256, exp} means a captured token cannot drive a different
// node, a different request body, or outlive its short window:
//   - node_id     — pins the target node (no cross-node reuse)
//   - body_sha256 — pins the EXACT /inference body (a captured token can't drive arbitrary inference)
//   - request_id  — correlates to the receipt; RecordReceipt's ON CONFLICT dedups any mint (replay D-i-D)
//   - exp         — short window (the gateway sets ~30s)
//
// CLOCK ASSUMPTION: exp is enforced against the node's wall clock with only a few seconds of skew
// (nodeAuthSkew) — this assumes gateway and node clocks are NTP-close, true on the controlled node
// network. It is a documented assumption, not a silent one.
type NodeAuthToken struct {
	NodeID     string `json:"node_id"`
	RequestID  string `json:"request_id"`
	BodySHA256 string `json:"body_sha256"` // hex-encoded sha256 of the /inference request body
	Exp        int64  `json:"exp"`         // unix seconds; node rejects now > exp + nodeAuthSkew
	Signature  []byte `json:"signature"`
}

// nodeAuthSkew is the few-seconds clock-skew grace (NTP-close assumption above) — seconds, not minutes.
const nodeAuthSkew = 5 * time.Second

// nodeAuthCanonicalPayload is the deterministic length-prefixed signed bytes:
// NodeID | RequestID | BodySHA256 | Exp (excludes Signature). Mirrors challengeCanonicalPayload.
func nodeAuthCanonicalPayload(nodeID, requestID, bodySHA256 string, exp int64) []byte {
	var buf []byte
	var l [8]byte
	put := func(s string) {
		binary.BigEndian.PutUint32(l[:4], uint32(len(s)))
		buf = append(buf, l[:4]...)
		buf = append(buf, s...)
	}
	put(nodeID)
	put(requestID)
	put(bodySHA256)
	binary.BigEndian.PutUint64(l[:], uint64(exp))
	buf = append(buf, l[:]...)
	return buf
}

// SignNodeAuthToken signs a request-bound token with the gateway's challenge private key and returns
// it as base64(JSON) for the X-Lens-Node-Token header.
func SignNodeAuthToken(lensPriv ed25519.PrivateKey, nodeID, requestID, bodySHA256 string, exp int64) (string, error) {
	tok := NodeAuthToken{NodeID: nodeID, RequestID: requestID, BodySHA256: bodySHA256, Exp: exp}
	tok.Signature = ed25519.Sign(lensPriv, nodeAuthCanonicalPayload(nodeID, requestID, bodySHA256, exp))
	b, err := json.Marshal(tok)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// VerifyNodeAuthToken decodes + verifies a token against the pinned Lens challenge pubkey AND its
// bindings: signature valid, node_id == wantNodeID, body_sha256 == wantBodySHA256, not expired.
// Returns nil iff all hold. The node calls this on POST /inference when X-Lens-Node-Token is present.
func VerifyNodeAuthToken(tokenB64 string, lensPub ed25519.PublicKey, wantNodeID, wantBodySHA256 string, now time.Time) error {
	if len(lensPub) != ed25519.PublicKeySize {
		return errors.New("povi: invalid lens public key length")
	}
	raw, err := base64.StdEncoding.DecodeString(tokenB64)
	if err != nil {
		return errors.New("povi: node-auth token not valid base64")
	}
	var tok NodeAuthToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return errors.New("povi: node-auth token not valid JSON")
	}
	if len(tok.Signature) != ed25519.SignatureSize {
		return errors.New("povi: invalid node-auth signature length")
	}
	if !ed25519.Verify(lensPub, nodeAuthCanonicalPayload(tok.NodeID, tok.RequestID, tok.BodySHA256, tok.Exp), tok.Signature) {
		return errors.New("povi: node-auth signature verification failed")
	}
	if subtle.ConstantTimeCompare([]byte(tok.NodeID), []byte(wantNodeID)) != 1 {
		return errors.New("povi: node-auth token node_id mismatch")
	}
	if subtle.ConstantTimeCompare([]byte(tok.BodySHA256), []byte(wantBodySHA256)) != 1 {
		return errors.New("povi: node-auth token body hash mismatch")
	}
	if now.Unix() > tok.Exp+int64(nodeAuthSkew/time.Second) {
		return errors.New("povi: node-auth token expired")
	}
	return nil
}
