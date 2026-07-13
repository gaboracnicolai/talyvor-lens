package outputverify_test

import (
	"testing"

	"github.com/talyvor/lens/internal/outputverify"
)

const (
	ws     = "ws-A"
	model  = "openai/gpt-4o"
	bucket = int64(1_700_000)
)

// Deterministic: identical server-observed inputs → identical output_id.
func TestIdentity_Deterministic(t *testing.T) {
	pH := outputverify.Sha256Hex([]byte("the prompt"))
	rH := outputverify.Sha256Hex([]byte("the response"))
	a := outputverify.DeriveOutputID(ws, model, pH, rH, bucket)
	b := outputverify.DeriveOutputID(ws, model, pH, rH, bucket)
	if a != b {
		t.Fatalf("non-deterministic: %s != %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("want a 32-byte hex sha256 (64 chars), got %d", len(a))
	}
}

// A DIFFERENT response ⇒ a DIFFERENT id (a workspace cannot substitute a good output for a bad one and keep
// the same identity). Same for workspace, model, prompt, and served-at bucket.
func TestIdentity_SensitiveToEveryField(t *testing.T) {
	base := outputverify.DeriveOutputID(ws, model,
		outputverify.Sha256Hex([]byte("P")), outputverify.Sha256Hex([]byte("R")), bucket)
	cases := map[string]string{
		"response":  outputverify.DeriveOutputID(ws, model, outputverify.Sha256Hex([]byte("P")), outputverify.Sha256Hex([]byte("R2")), bucket),
		"prompt":    outputverify.DeriveOutputID(ws, model, outputverify.Sha256Hex([]byte("P2")), outputverify.Sha256Hex([]byte("R")), bucket),
		"workspace": outputverify.DeriveOutputID("ws-B", model, outputverify.Sha256Hex([]byte("P")), outputverify.Sha256Hex([]byte("R")), bucket),
		"model":     outputverify.DeriveOutputID(ws, "anthropic/claude", outputverify.Sha256Hex([]byte("P")), outputverify.Sha256Hex([]byte("R")), bucket),
		"bucket":    outputverify.DeriveOutputID(ws, model, outputverify.Sha256Hex([]byte("P")), outputverify.Sha256Hex([]byte("R")), bucket+1),
	}
	for field, got := range cases {
		if got == base {
			t.Errorf("id must change when %s changes, but it did not", field)
		}
	}
}

// UNREPUDIABLE: given the stored prompt/response HASHES, anyone can recompute the binding — a workspace
// cannot deny producing an output that hashes to its id.
func TestIdentity_Unrepudiable_RecomputesFromHashes(t *testing.T) {
	prompt := []byte("summarise this")
	response := []byte(`{"summary":"ok"}`)
	stored := outputverify.DeriveOutputID(ws, model, outputverify.Sha256Hex(prompt), outputverify.Sha256Hex(response), bucket)
	// An auditor holding only (ws, model, promptHash, responseHash, bucket) recomputes the SAME id.
	recomputed := outputverify.DeriveOutputID(ws, model, outputverify.Sha256Hex(prompt), outputverify.Sha256Hex(response), bucket)
	if recomputed != stored {
		t.Fatalf("binding must recompute from stored hashes; %s != %s", recomputed, stored)
	}
	// Repudiation attempt: swapping in a DIFFERENT response yields a DIFFERENT id → cannot masquerade.
	if outputverify.DeriveOutputID(ws, model, outputverify.Sha256Hex(prompt), outputverify.Sha256Hex([]byte(`{"summary":"tampered"}`)), bucket) == stored {
		t.Error("a different response must not reproduce the stored id")
	}
}

// A hostile client X-Talyvor-Request-ID cannot influence the id: it is NOT a parameter of DeriveOutputID
// (the signature accepts only server-observed values — the compiler enforces this), and the proxy off-path
// test (TestOutputVerdict_OffPath_ZeroCost_And_Records) proves captureOutputVerdict derives the id from
// server values without reading any header. Here we pin the length-prefixing that defeats field-boundary
// collisions: ("ab","c") and ("a","bc") must NOT collide.
func TestIdentity_NoBoundaryCollision(t *testing.T) {
	pH, rH := outputverify.Sha256Hex([]byte("P")), outputverify.Sha256Hex([]byte("R"))
	if outputverify.DeriveOutputID("ab", "c", pH, rH, bucket) == outputverify.DeriveOutputID("a", "bc", pH, rH, bucket) {
		t.Error("length-prefixing must prevent ('ab','c') vs ('a','bc') collision")
	}
}
