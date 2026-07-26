package poolsafety_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/poolsafety"
)

// WHAT THIS GUARDS
//
// Cross-tenant pooling serves one workspace's cached RESPONSE to another when their prompt
// embeddings exceed LENS_SEMANTIC_THRESHOLD (0.92). The safety of that rests on an
// assumption nobody had tested: that two unrelated tenants' prompts land far apart.
//
// They do NOT automatically. Lens embeds every message concatenated
// (internal/proxy/proxy.go extractPrompt), and real clients prepend a large fixed system
// preamble to every request — talyvor-code ships a ~720-char one, LangChain/Cursor/Continue
// and most agent frameworks are comparable or larger. That shared text is a big share of
// what gets embedded, and it pushes unrelated requests together.
//
// MEASURED, against real embedding models, on two unrelated codebases (a Go payments
// ledger and a Python imaging pipeline) wrapped in the same shipped review preamble:
//
//	text-embedding-3-small (what Lens uses) ..... 0.69   safe — 0.23 below threshold
//	all-MiniLM-L6-v2 ........................... 0.84   uncomfortable
//	BAAI/bge-small-en-v1.5 ..................... 0.985  WOULD LEAK
//
// Same code, same prompts, same threshold — three different answers depending on which
// model LENS_EMBEDDING_MODEL happens to name. A safety property that depends on a config
// value nobody thinks of as security-relevant is not a safety property; it is luck that
// has held so far. This package makes the dependency CHECKED.
//
// These tests are the positive control on the checker. They use stub embedders with known
// geometry, so they run offline in CI and prove the checker actually fires. The checker is
// then run against the REAL configured embedder at deploy preflight, where a key exists.

// hashEmbedder is a deterministic stand-in with a WIDE dynamic range: it hashes the text
// into a sparse direction, so unrelated inputs are near-orthogonal. Models like
// text-embedding-3-small behave this way, and this stub must PASS the check.
type hashEmbedder struct{}

func (hashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	sum := sha256.Sum256([]byte(text))
	v := make([]float32, 64)
	for i, b := range sum {
		v[int(b)%64] += float32(i%7) + 1
	}
	return normalise(v), nil
}

// flatEmbedder simulates a COMPRESSED-range model — the bge-small failure mode. Every
// input maps close to a constant direction with only a small text-dependent perturbation,
// so even unrelated texts score high. This stub must FAIL the check; if it does not, the
// checker cannot detect the situation it exists for.
type flatEmbedder struct{}

func (flatEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	sum := sha256.Sum256([]byte(text))
	v := make([]float32, 64)
	for i := range v {
		v[i] = 1.0 // the dominant shared component
	}
	for i, b := range sum[:8] {
		v[i] += float32(b) / 255.0 * 0.05 // tiny text-dependent wobble
	}
	return normalise(v), nil
}

type failingEmbedder struct{}

func (failingEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("embedder unavailable")
}

func normalise(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	n = math.Sqrt(n)
	if n == 0 {
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
	return v
}

// A wide-range embedder keeps unrelated tenants apart: the check passes.
func TestCheck_WideRangeEmbedder_Passes(t *testing.T) {
	rep, err := poolsafety.Check(context.Background(), hashEmbedder{}, 0.92)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !rep.OK {
		t.Fatalf("a wide-range embedder was reported UNSAFE — the checker is too strict to be usable:\n%s", rep)
	}
	if len(rep.Pairs) == 0 {
		t.Fatal("no pairs evaluated — the corpus is empty, so the check proves nothing")
	}
}

// THE POSITIVE CONTROL. A compressed-range embedder is exactly the bge-small situation,
// where two unrelated codebases scored 0.985. The checker MUST fail here.
func TestCheck_CompressedRangeEmbedder_Fails(t *testing.T) {
	rep, err := poolsafety.Check(context.Background(), flatEmbedder{}, 0.92)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rep.OK {
		t.Fatalf("a compressed-range embedder was reported SAFE. This is the bge-small case "+
			"(0.985 between unrelated codebases); if the checker cannot see it, it cannot see "+
			"the thing it exists for.\n%s", rep)
	}
	if rep.Worst.Similarity < 0.92 {
		t.Errorf("worst pair scored %.4f, below the threshold — the failure was reported for the wrong reason",
			rep.Worst.Similarity)
	}
}

// The corpus must actually contain preamble-sharing pairs, or the check is decorative.
func TestCorpus_PairsShareAPreambleAndDifferInContent(t *testing.T) {
	for _, p := range poolsafety.Corpus() {
		if p.Preamble == "" {
			t.Errorf("%s: empty preamble — this pair does not exercise the shared-boilerplate effect", p.Name)
		}
		if p.A == p.B {
			t.Errorf("%s: the two payloads are identical, so a high score would be correct, not a finding", p.Name)
		}
		if !strings.Contains(p.Full(p.A), p.Preamble) || !strings.Contains(p.Full(p.B), p.Preamble) {
			t.Errorf("%s: the preamble is not present in both rendered prompts", p.Name)
		}
	}
}

// An embedder error must surface, not be silently reported as safe — the failure mode that
// would turn this guard into decoration.
func TestCheck_EmbedderError_Surfaces(t *testing.T) {
	if _, err := poolsafety.Check(context.Background(), failingEmbedder{}, 0.92); err == nil {
		t.Fatal("an embedder failure was swallowed; the check must not report safety it did not measure")
	}
}

// A threshold of 0 would make every pair a violation; a threshold above 1 makes none. Both
// are configuration mistakes worth catching, since the threshold is operator-settable.
func TestCheck_ImplausibleThreshold_Rejected(t *testing.T) {
	for _, th := range []float64{0, -1, 1.5} {
		if _, err := poolsafety.Check(context.Background(), hashEmbedder{}, th); err == nil {
			t.Errorf("threshold %v accepted; an out-of-range threshold must be rejected rather than silently reported", th)
		}
	}
}
