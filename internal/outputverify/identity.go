// Package outputverify is the K4 gateway-bound output-identity + INTRINSIC verifier. At serve time Lens
// already holds the request body (the constraint the caller declared) AND the response body (what the model
// produced), in the same place. This package derives a SERVER-BOUND identity for that output and — reusing
// eval.StaticScore's intrinsic methods — records whether the output VIOLATES a constraint the REQUEST
// ITSELF declared. It NEVER compares one tenant to another, NEVER moves money, and NEVER blocks or alters
// the response (off-path). It is import-guarded against every ledger/mint package. This produces a VERDICT,
// nothing else. DEFAULT-OFF at the wiring site.
package outputverify

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

// Sha256Hex is hex(sha256(b)). Used for the prompt/response EVIDENCE hashes — the content is hashed, never
// stored raw (a raw-content table is out of scope and must not exist).
func Sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// writeField length-prefixes each field (8-byte big-endian length ‖ bytes) before hashing, so no two
// distinct field tuples can collide by concatenation ambiguity (e.g. ("ab","c") vs ("a","bc")).
func writeField(h hash.Hash, b []byte) {
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(b)))
	_, _ = h.Write(l[:])
	_, _ = h.Write(b)
}

// DeriveOutputID computes the server-derived, gateway-bound output identity. Every input is a value the
// GATEWAY observed at serve time — NEVER a caller-supplied header — so a workspace can neither FORGE an id
// for an output it did not produce nor REPUDIATE one it did: given the stored prompt/response hashes, the
// binding recomputes exactly. Because the client's X-Talyvor-Request-ID is not an input, a hostile header
// cannot influence the id.
//
//	output_id = sha256( "k4id/v1" ‖ workspace_id ‖ model ‖ promptSHA256 ‖ responseSHA256 ‖ servedAtBucket )
func DeriveOutputID(workspaceID, model, promptSHA256, responseSHA256 string, servedAtBucket int64) string {
	h := sha256.New()
	writeField(h, []byte("k4id/v1")) // domain separation / version
	writeField(h, []byte(workspaceID))
	writeField(h, []byte(model))
	writeField(h, []byte(promptSHA256))
	writeField(h, []byte(responseSHA256))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(servedAtBucket))
	writeField(h, buf[:])
	return hex.EncodeToString(h.Sum(nil))
}
