package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/talyvor/lens/internal/safehttp"
)

// newNodeHTTPClient builds the *http.Client Lens uses when contacting
// registered nodes (inference probes, embedding probes, PoVI challenges).
//
// When skipVerify is true, TLS certificate verification is disabled so
// that nodes presenting self-signed certificates — the recommended
// default for LAN deployments — are accepted without a custom CA bundle.
// Only use this on controlled private networks; set
// LENS_NODE_TLS_SKIP_VERIFY=true to opt in.
//
// The returned transport always carries an explicit TLSClientConfig so
// that future support for LENS_NODE_TLS_CA (custom CA bundle) can be
// wired here without touching each call site.
func newNodeHTTPClient(skipVerify bool, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// B-SSRF-Lens: refuse to dial loopback/private/link-local/metadata (169.254.169.254) — a
			// node URL is caller-supplied at registration, so without this it is an SSRF vector. Keeps
			// the node TLS config. NOTE: this also blocks RFC1918 private ranges; if LAN/private-network
			// nodes are used (the opt-in LENS_NODE_TLS_SKIP_VERIFY=true mode), that needs a per-deployment
			// allowlist — see internal/safehttp doc + the PR.
			DialContext:       safehttp.SafeDialContext(&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}),
			ForceAttemptHTTP2: true,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: skipVerify, //nolint:gosec // opt-in, controlled private networks only
			},
		},
		CheckRedirect: safehttp.CheckRedirect,
	}
}
