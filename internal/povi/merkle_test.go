package povi

import "testing"

// Same trace → same root (deterministic); a changed step → different root.
func TestMerkleRoot_Deterministic(t *testing.T) {
	trace := [][]byte{[]byte("tok-a"), []byte("tok-b"), []byte("tok-c")}
	r1 := MerkleRoot(trace)
	r2 := MerkleRoot([][]byte{[]byte("tok-a"), []byte("tok-b"), []byte("tok-c")})
	if r1 != r2 {
		t.Fatalf("same trace produced different roots: %x vs %x", r1, r2)
	}
	changed := MerkleRoot([][]byte{[]byte("tok-a"), []byte("tok-X"), []byte("tok-c")})
	if r1 == changed {
		t.Error("changing a step must change the root")
	}
}

// A single-leaf tree's root is the leaf hash.
func TestMerkleRoot_SingleLeaf(t *testing.T) {
	leaf := []byte("only")
	if MerkleRoot([][]byte{leaf}) != LeafHash(leaf) {
		t.Error("single-leaf root must equal the leaf hash")
	}
}

// Leaf and internal-node hashing are domain-separated, so a leaf value can
// never be confused with an internal node (second-preimage protection).
func TestLeafAndNodeDomainSeparation(t *testing.T) {
	a, b := LeafHash([]byte("x")), LeafHash([]byte("y"))
	// nodeHash(a,b) must not collide with any LeafHash of the concatenation.
	n := nodeHash(a, b)
	concat := append(append([]byte{}, a[:]...), b[:]...)
	if n == LeafHash(concat) {
		t.Error("internal node hash collides with a leaf hash — missing domain separation")
	}
}

// A valid authentication path verifies; a tampered sibling or leaf fails.
func TestVerifyPath_ValidAndTampered(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 8, 9} {
		trace := make([][]byte, n)
		for i := range trace {
			trace[i] = []byte{byte('a' + i)}
		}
		root := MerkleRoot(trace)
		for idx := 0; idx < n; idx++ {
			proof, err := BuildProof(trace, idx)
			if err != nil {
				t.Fatalf("n=%d idx=%d BuildProof: %v", n, idx, err)
			}
			if !VerifyPath(root, trace[idx], proof) {
				t.Errorf("n=%d idx=%d: valid path failed to verify", n, idx)
			}
			// Tamper the leaf → must fail.
			if VerifyPath(root, []byte("tampered"), proof) {
				t.Errorf("n=%d idx=%d: tampered leaf verified", n, idx)
			}
			// Tamper a sibling (if any) → must fail.
			if len(proof.Siblings) > 0 {
				bad := proof
				bad.Siblings = append([][32]byte{}, proof.Siblings...)
				bad.Siblings[0][0] ^= 0xFF
				if VerifyPath(root, trace[idx], bad) {
					t.Errorf("n=%d idx=%d: tampered sibling verified", n, idx)
				}
			}
			// A proof against the wrong root must fail.
			var wrong [32]byte
			copy(wrong[:], root[:])
			wrong[0] ^= 0xFF
			if VerifyPath(wrong, trace[idx], proof) {
				t.Errorf("n=%d idx=%d: verified against wrong root", n, idx)
			}
		}
	}
}

func TestBuildProof_OutOfRange(t *testing.T) {
	trace := [][]byte{[]byte("a"), []byte("b")}
	if _, err := BuildProof(trace, 2); err == nil {
		t.Error("BuildProof with out-of-range index must error")
	}
	if _, err := BuildProof(trace, -1); err == nil {
		t.Error("BuildProof with negative index must error")
	}
}
