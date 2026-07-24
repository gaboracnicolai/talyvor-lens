package main

import (
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// clientIPMiddleware resolves the calling client's IP address for this
// deployment's topology. Wired in run() as the single client-IP authority;
// read it back with clientIP.
//
// Replaces chi's middleware.RealIP (GO-2026-5774 / GO-2026-5775 /
// GO-2026-5777), which set r.RemoteAddr from the LEFTMOST X-Forwarded-For
// value — the one the client supplies — or from True-Client-IP / X-Real-IP
// whether or not our infrastructure sets them. Upgrading chi does NOT fix
// this. RealIP's body is byte-for-byte unchanged at v5.3.1 — v5.3.0 "fixed"
// the advisory by adding this ClientIPFrom* family and deprecating RealIP, so
// a bump alone silences govulncheck while leaving the spoof fully open. The
// call site had to change; see TestNoRealIPAnywhere, which is now the only
// guard against reintroduction.
//
// WHY ClientIPFromXFF WITH NO ARGUMENTS — this is topology-specific and there
// is no safe default. Behaviour below was verified empirically against
// caddy:2-alpine (v2.11.4), the image deploy/docker-compose.yaml runs, with
// our own bare `reverse_proxy` Caddyfile — not taken from documentation:
//
//   - lens sits behind a containerised Caddy on the same Docker bridge.
//     deploy/caddy/Caddyfile is a bare `reverse_proxy lens:8080`, so Caddy
//     runs its defaults, which means `trusted_proxies` is unset — it trusts
//     no inbound hop.
//   - Consequently Caddy REPLACES X-Forwarded-For outright with the true TCP
//     peer. A client-supplied chain, comma-list or duplicate header alike, is
//     discarded before lens ever sees it. XFF therefore arrives with exactly
//     one entry, and it is trustworthy.
//   - Caddy does NOT set, strip or normalise X-Real-IP or True-Client-IP.
//     Those pass through from the client verbatim, so their presence is by
//     definition a forgery attempt and they must never be read. This — not
//     the leftmost-XFF issue — was the live spoof vector here: RealIP checked
//     True-Client-IP FIRST, ahead of XFF, so Caddy's XFF sanitising was
//     bypassed entirely.
//   - Caddy terminates TLS itself via ACME, so it is the internet-facing
//     front door with no CDN in front of it. Exactly one trusted hop.
//   - No-argument ClientIPFromXFF returns the RIGHTMOST XFF entry: the one
//     Caddy wrote. Fail-closed if it does not parse. This is correct whether
//     Caddy replaces (today) or appends (what it would do if `trusted_proxies`
//     were ever configured), which is why it is preferred over any
//     leftmost-reading scheme.
//
// Rejected alternatives:
//
//   - ClientIPFromRemoteAddr — would resolve every request to Caddy's bridge
//     address (e.g. 172.18.0.3), collapsing all clients onto one value.
//   - ClientIPFromHeader("X-Real-IP") — Caddy does not set it; without a
//     matching Caddyfile `header_up`, every request would silently resolve to
//     "". It would also couple this call site to a config change in another
//     deploy artifact, which can be missed.
//   - ClientIPFromXFF(<docker bridge CIDR>) — actively harmful here. The
//     bridge address never appears IN the XFF chain (it is the TCP peer), so
//     the CIDR buys nothing; but a caller legitimately originating inside the
//     bridge range would be skipped as a "trusted hop" and we would walk left
//     into client-supplied values — reintroducing the spoof for internal
//     callers.
//   - ClientIPFromXFFTrustedProxies(1) — behaviourally identical here, but
//     documented as brittle: it silently starts trusting a forged value if a
//     proxy level is ever added.
//
// If a second hop is ever put in front of Caddy (a CDN, another LB), the
// rightmost entry becomes THAT hop's address and this must be revisited —
// switch to ClientIPFromXFF with the new front door's CIDRs.
func clientIPMiddleware() func(http.Handler) http.Handler {
	return middleware.ClientIPFromXFF()
}

// clientIP returns the client IP that Lens resolved for this request, or ""
// if none could be established (no Caddy hop, or an unparseable value —
// both fail closed).
//
// This is the ONLY sanctioned way to obtain a caller's network origin. Do not
// read r.RemoteAddr for this: behind Caddy it is the proxy's bridge address,
// not the client's. Nothing in Lens keys off a client IP today — rate limits
// key off (workspace, key ID) and the anti-Sybil ring detector keys off card
// fingerprints and owner keys, all server-derived — so this reader exists to
// make the *next* IP consumer safe by construction.
func clientIP(r *http.Request) string {
	return middleware.GetClientIP(r.Context())
}
