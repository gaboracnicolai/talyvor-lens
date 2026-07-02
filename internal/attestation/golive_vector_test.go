package attestation

import (
	"context"
	"crypto/x509"
	"os"
	"strconv"
	"testing"
	"time"
)

// (proof 8) GO-LIVE GATE — the ONE test that proves the REAL NVIDIA trust chain (not a self-generated CA).
// It is SKIPPED in CI: it needs a real NRAS-signed EAT (LENS_TEST_NVIDIA_EAT), the real NVIDIA root CA PEM
// (LENS_NVIDIA_ROOT_CA_PEM), and the live NRAS JWKS (LENS_NVIDIA_JWKS_URL) — all absent in CI (network +
// token expiry). The CI proofs verify the LOGIC against a test CA; THIS proves the production anchor and
// must be run before flipping the mint on (step c). Its existence + skip is the documented gate.
func TestVerify_RealNVIDIAVector_GoLiveGate(t *testing.T) {
	rawEAT := os.Getenv("LENS_TEST_NVIDIA_EAT")
	rootPEM := os.Getenv("LENS_NVIDIA_ROOT_CA_PEM")
	jwksURL := os.Getenv("LENS_NVIDIA_JWKS_URL")
	if rawEAT == "" || rootPEM == "" || jwksURL == "" {
		t.Skip("go-live gate: set LENS_TEST_NVIDIA_EAT + LENS_NVIDIA_ROOT_CA_PEM + LENS_NVIDIA_JWKS_URL to verify against the REAL NVIDIA chain (skipped in CI)")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(rootPEM)) {
		t.Fatal("LENS_NVIDIA_ROOT_CA_PEM did not parse")
	}
	v := NewVerifier(roots, NewHTTPJWKS(jwksURL, nil, time.Now), time.Now)
	// The real token carries its own eat_nonce; a caller would pass the nonce it issued. Here we just prove
	// the signature + real x5c chain + claims validate against NVIDIA's production root.
	nonce := parseEATNonceOrSkip(t, rawEAT)
	res, err := v.Verify(context.Background(), rawEAT, nonce, nil)
	if err != nil {
		t.Fatalf("real NVIDIA EAT must verify against the production root: %v", err)
	}
	if !res.CCMode {
		t.Fatalf("real EAT cc-mode should be true for a CC node: %+v", res)
	}
	t.Logf("GO-LIVE gate PASSED against the real NVIDIA chain: gpu_class=%q key_bound=%v", res.GPUClass, res.KeyBound)
}

// parseEATNonceOrSkip reads eat_nonce from an unverified token so the gate test can echo it (the real token
// fixes its own nonce). Kept trivial — the actual verification is what matters.
func parseEATNonceOrSkip(t *testing.T, _ string) int64 {
	t.Helper()
	if v := os.Getenv("LENS_TEST_NVIDIA_EAT_NONCE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	t.Skip("go-live gate: set LENS_TEST_NVIDIA_EAT_NONCE to the token's eat_nonce")
	return 0
}
