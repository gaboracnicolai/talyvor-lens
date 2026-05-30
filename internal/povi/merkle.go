package povi

import (
	"crypto/sha256"
	"errors"
)

// Merkle commitment over a generation trace. Pure stdlib sha256.
//
// Domain separation: leaf hashes are prefixed with 0x00 and internal-node
// hashes with 0x01, so an attacker can never present an internal node as a
// leaf (or vice versa) — the classic Merkle second-preimage defense.
//
// NOTE ON LEAVES (honesty): in this part the node feeds ONE LEAF PER OUTPUT
// RUNE of the response (a documented stand-in), NOT one leaf per model token.
// The commitment is real and tamper-evident, but it commits to the response's
// runes, not the model's internal token steps, until per-token streaming is
// wired into the provider interface. The tree/path machinery here is leaf-
// agnostic — it works identically whatever a leaf represents — so switching to
// true per-token leaves later changes only the feeding, not this structure.

const (
	leafPrefix     = 0x00
	internalPrefix = 0x01
)

// LeafHash hashes one trace step into a Merkle leaf (domain-prefixed).
func LeafHash(data []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{leafPrefix})
	h.Write(data)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// nodeHash combines two child hashes into a parent (domain-prefixed).
func nodeHash(left, right [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{internalPrefix})
	h.Write(left[:])
	h.Write(right[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// buildLevels constructs every level of the tree bottom-up. level[0] is the
// leaf hashes; the final level is a single root. An odd node at any level is
// paired with itself (standard duplication), so the structure is fully
// determined by the leaf sequence.
func buildLevels(trace [][]byte) [][][32]byte {
	if len(trace) == 0 {
		return nil
	}
	level := make([][32]byte, len(trace))
	for i, step := range trace {
		level[i] = LeafHash(step)
	}
	levels := [][][32]byte{level}
	for len(level) > 1 {
		next := make([][32]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			left := level[i]
			right := left // odd tail pairs with itself
			if i+1 < len(level) {
				right = level[i+1]
			}
			next = append(next, nodeHash(left, right))
		}
		levels = append(levels, next)
		level = next
	}
	return levels
}

// MerkleRoot returns the root over a trace. An empty trace yields the zero root.
func MerkleRoot(trace [][]byte) [32]byte {
	levels := buildLevels(trace)
	if len(levels) == 0 {
		return [32]byte{}
	}
	return levels[len(levels)-1][0]
}

// Proof is a sampled authentication path: the sibling hashes from a leaf up to
// the root, with SiblingRight[i] telling which side the sibling sits on.
type Proof struct {
	LeafIndex    int        `json:"leaf_index"`
	NumLeaves    int        `json:"num_leaves"`
	Siblings     [][32]byte `json:"siblings"`
	SiblingRight []bool     `json:"sibling_right"` // true = sibling is the right child
}

// BuildProof produces the authentication path for the leaf at index.
func BuildProof(trace [][]byte, index int) (Proof, error) {
	if index < 0 || index >= len(trace) {
		return Proof{}, errors.New("povi: leaf index out of range")
	}
	levels := buildLevels(trace)
	p := Proof{LeafIndex: index, NumLeaves: len(trace)}
	idx := index
	for lvl := 0; lvl < len(levels)-1; lvl++ {
		cur := levels[lvl]
		var sib [32]byte
		var sibRight bool
		if idx%2 == 0 {
			// We're the left child; sibling is on the right (or ourselves if odd tail).
			if idx+1 < len(cur) {
				sib = cur[idx+1]
			} else {
				sib = cur[idx]
			}
			sibRight = true
		} else {
			sib = cur[idx-1]
			sibRight = false
		}
		p.Siblings = append(p.Siblings, sib)
		p.SiblingRight = append(p.SiblingRight, sibRight)
		idx /= 2
	}
	return p, nil
}

// VerifyPath recomputes the root from a leaf + its authentication path and
// reports whether it matches the committed root.
func VerifyPath(root [32]byte, leaf []byte, proof Proof) bool {
	if len(proof.Siblings) != len(proof.SiblingRight) {
		return false
	}
	h := LeafHash(leaf)
	for i, sib := range proof.Siblings {
		if proof.SiblingRight[i] {
			h = nodeHash(h, sib)
		} else {
			h = nodeHash(sib, h)
		}
	}
	return h == root
}
