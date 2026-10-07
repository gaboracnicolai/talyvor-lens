package embedder

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// HashedDims is the length of a HashedEmbedder's vectors.
const HashedDims = 256

// HashedEmbedder is a deterministic embedder that never leaves the process: each word of the text, lower-cased, is
// hashed into one of HashedDims buckets with a sign, and the counts are scaled to unit length. Two texts that share most
// of their words lie close together. CI uses it in place of the embeddings API, and Lens uses it when no OpenAI key is
// configured, so the marketplace's similarity check (B32.46) still compares wording.
type HashedEmbedder struct{}

// NewHashedEmbedder returns the offline embedder.
func NewHashedEmbedder() HashedEmbedder { return HashedEmbedder{} }

// Embed answers text's vector; a text without words answers a zero vector.
func (HashedEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, HashedDims)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(w))
		sum := h.Sum64()
		if sum>>63 == 0 {
			v[sum%HashedDims]++
		} else {
			v[sum%HashedDims]--
		}
	}
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm == 0 {
		return v, nil
	}
	scale := float32(1 / math.Sqrt(norm))
	for i := range v {
		v[i] *= scale
	}
	return v, nil
}

// Model names the space a HashedEmbedder's vectors live in.
func (HashedEmbedder) Model() string { return "talyvor-hashed-words-256" }
