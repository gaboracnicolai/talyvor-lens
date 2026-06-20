package povi

// LeafKind records what each Merkle leaf in a committed trace REPRESENTS. The
// tree/proof machinery (LeafHash, MerkleRoot, BuildProof, VerifyPath) is
// leaf-agnostic — it hashes whatever bytes it is fed — so the kind only labels
// granularity for the Part-3 challenge and the audit trail; it never changes how
// a root is folded or a path is verified.
type LeafKind string

const (
	// LeafKindRune is the documented stand-in: one leaf per OUTPUT RUNE. It is the
	// honest ceiling for backends that expose no token boundaries (every local
	// provider today), where there is no token sequence to commit to.
	LeafKindRune LeafKind = "rune"

	// LeafKindToken is true per-model-token granularity: one leaf per token. It is
	// fed from a token sequence — synthetic in tests today, and live if/when the
	// provider interface grows token streaming (out of scope here). The Merkle
	// structure is identical; only the feeding differs.
	LeafKindToken LeafKind = "token"
)

// StepsFromRunes produces one trace step per output rune — the LeafKindRune
// feeding. A multi-rune string yields one leaf per rune.
func StepsFromRunes(s string) [][]byte {
	steps := make([][]byte, 0, len(s))
	for _, r := range s {
		steps = append(steps, []byte(string(r)))
	}
	return steps
}

// StepsFromTokens produces one trace step per model token — the LeafKindToken
// feeding. A sequence of N tokens yields exactly N leaves, regardless of how many
// runes each token spans (the per-token property the rune stand-in lacks).
func StepsFromTokens(tokens []string) [][]byte {
	steps := make([][]byte, 0, len(tokens))
	for _, tok := range tokens {
		steps = append(steps, []byte(tok))
	}
	return steps
}
