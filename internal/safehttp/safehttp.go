// Package safehttp hardens outbound HTTP against SSRF: its dialer resolves the target host and refuses
// to connect to loopback, private (RFC1918 / ULA), link-local (169.254/16 incl. the cloud metadata
// endpoint 169.254.169.254, fe80::/10), unspecified, and multicast addresses, re-validating every
// redirect hop. Ported from talyvor-track's SEC-6 fix. Use for every fetch of a caller/config-supplied
// URL — node registration, the audit webhook, node inference/attestation/challenge/benchprobe calls.
//
// NOTE (deployment): this blocks ALL private ranges. If Lens mining nodes legitimately register on a
// private cluster network (10/192.168), that needs a per-deployment allowlist — a documented follow-on.
// The loopback / link-local / metadata blocks are unambiguous and never a legitimate node target.
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ErrBlockedAddress is returned when a fetch targets a non-public (SSRF-sensitive) address.
var ErrBlockedAddress = errors.New("safehttp: refusing to connect to a non-public address")

// blocked reports whether ip is in an SSRF-sensitive range that must never be dialed.
func blocked(ip net.IP) bool {
	return ip.IsLoopback() || // 127.0.0.0/8, ::1
		ip.IsPrivate() || // 10/8, 172.16/12, 192.168/16, fc00::/7 (ULA)
		ip.IsLinkLocalUnicast() || // 169.254.0.0/16 (incl. metadata 169.254.169.254), fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() || // 0.0.0.0, ::
		ip.IsMulticast()
}

// SafeDialContext resolves the host, rejects any resolved address in a blocked range, and dials the
// resolved IP directly (so a DNS name can't rebind to an internal IP between the check and the connect).
// Exported so callers that need their own Transport (e.g. one carrying a node TLS config) can install
// the SSRF guard without losing that config.
func SafeDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if base == nil {
		base = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("safehttp: no addresses for %q", host)
		}
		for _, ip := range ips {
			if blocked(ip) {
				return nil, fmt.Errorf("%w: %s resolves to %s", ErrBlockedAddress, host, ip)
			}
		}
		return base.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
}

// CheckRedirect re-validates each redirect hop (the new connection re-dials through SafeDialContext) and
// caps the chain. Install it on any client that follows redirects to a caller-supplied URL.
func CheckRedirect(_ *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("safehttp: stopped after 10 redirects")
	}
	return nil
}

// Client returns an *http.Client with the SSRF dial guard on the initial request and every redirect hop.
func Client(timeout time.Duration) *http.Client {
	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           SafeDialContext(base),
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: CheckRedirect,
	}
}
