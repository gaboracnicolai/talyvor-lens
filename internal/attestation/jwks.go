// Package attestation is the gateway-side verifier for Proof-of-Confidential-Compute (step b): it receives a
// node's NVIDIA NRAS EAT (Entity Attestation Token, a JWT) and verifies it CRYPTOGRAPHICALLY — the JWT
// signature via golang-jwt, the signing cert's x5c chain to the PINNED NVIDIA root CA via stdlib
// crypto/x509, then the EAT claims (iss/exp/cc-mode/measurements/eat_nonce) + optional report_data
// key-binding. No custom signature crypto: stdlib + golang-jwt only. Records the verified hardware class to
// node_attestations; mints nothing (step c is the mint).
package attestation

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

var errUnknownKID = errors.New("attestation: no JWKS key for kid")

// JWKSSource resolves a JWT `kid` to its signing leaf certificate + the intermediates needed to chain it to
// the pinned root. Hand-rolled over x5c (no external JWKS lib) — the leaf's public key verifies the JWT
// signature, the chain is validated to the pinned NVIDIA root by the Verifier.
type JWKSSource interface {
	CertForKID(ctx context.Context, kid string) (leaf *x509.Certificate, intermediates *x509.CertPool, err error)
}

// jwk is one JSON Web Key with an x5c cert chain (base64-STD DER, leaf first). We use x5c exclusively — the
// EAT's trust is a cert chain to NVIDIA's root, not a bare public key.
type jwk struct {
	Kid string   `json:"kid"`
	X5c []string `json:"x5c"`
}

type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

// HTTPJWKS fetches + caches an NRAS JWKS. On an unknown kid it re-fetches ONCE (a key rotation must not
// silently fail against a stale cache), then gives up. Concurrency-safe.
type HTTPJWKS struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu         sync.Mutex
	cache      map[string]*certChain // kid → parsed chain
	fetchedAt  time.Time
	minRefetch time.Duration
}

type certChain struct {
	leaf          *x509.Certificate
	intermediates *x509.CertPool
}

// NewHTTPJWKS builds an NRAS JWKS source. url is NVIDIA's published JWKS endpoint; the client should have a
// short timeout. minRefetch rate-limits re-fetches so a flood of unknown kids can't hammer NVIDIA.
func NewHTTPJWKS(url string, client *http.Client, now func() time.Time) *HTTPJWKS {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	return &HTTPJWKS{url: url, client: client, now: now, cache: map[string]*certChain{}, minRefetch: time.Minute}
}

func (h *HTTPJWKS) CertForKID(ctx context.Context, kid string) (*x509.Certificate, *x509.CertPool, error) {
	h.mu.Lock()
	c, ok := h.cache[kid]
	stale := h.now().Sub(h.fetchedAt) >= h.minRefetch
	h.mu.Unlock()
	if ok {
		return c.leaf, c.intermediates, nil
	}
	if !stale {
		return nil, nil, errUnknownKID // rate-limit: don't refetch on every miss
	}
	if err := h.refetch(ctx); err != nil {
		return nil, nil, err
	}
	h.mu.Lock()
	c, ok = h.cache[kid]
	h.mu.Unlock()
	if !ok {
		return nil, nil, errUnknownKID
	}
	return c.leaf, c.intermediates, nil
}

func (h *HTTPJWKS) refetch(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("attestation: JWKS fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("attestation: JWKS status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	parsed, err := parseJWKS(raw)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.cache = parsed
	h.fetchedAt = h.now()
	h.mu.Unlock()
	return nil
}

// parseJWKS turns a JWKS doc into kid→chain, parsing each key's x5c (leaf first, then intermediates).
func parseJWKS(raw []byte) (map[string]*certChain, error) {
	var doc jwksDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("attestation: JWKS decode: %w", err)
	}
	out := make(map[string]*certChain, len(doc.Keys))
	for _, k := range doc.Keys {
		if len(k.X5c) == 0 {
			continue
		}
		leaf, inter, err := parseX5C(k.X5c)
		if err != nil {
			return nil, err
		}
		out[k.Kid] = &certChain{leaf: leaf, intermediates: inter}
	}
	return out, nil
}

// parseX5C decodes a base64-STD DER x5c list (leaf first) into (leaf, intermediates pool).
func parseX5C(x5c []string) (*x509.Certificate, *x509.CertPool, error) {
	var leaf *x509.Certificate
	inter := x509.NewCertPool()
	for i, b64 := range x5c {
		der, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, nil, fmt.Errorf("attestation: x5c[%d] not base64: %w", i, err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, nil, fmt.Errorf("attestation: x5c[%d] not a cert: %w", i, err)
		}
		if i == 0 {
			leaf = cert
		} else {
			inter.AddCert(cert)
		}
	}
	if leaf == nil {
		return nil, nil, errors.New("attestation: x5c has no leaf")
	}
	return leaf, inter, nil
}
