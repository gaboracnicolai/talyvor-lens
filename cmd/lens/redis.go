package main

import (
	"crypto/tls"

	"github.com/redis/go-redis/v9"
)

// applyRedisTLS ensures TLS is active on opts and applies the skip-verify
// flag when requested.
//
// Callers should invoke this when either:
//   - the URL used rediss:// (go-redis already set TLSConfig — we preserve
//     that config and only layer in InsecureSkipVerify), or
//   - LENS_REDIS_TLS=true was set (opts.TLSConfig is nil — we create a new
//     config with MinVersion TLS 1.2 before applying InsecureSkipVerify).
//
// skipVerify disables server certificate verification.  Only appropriate
// for self-signed / internal-CA certificates in controlled environments.
func applyRedisTLS(opts *redis.Options, skipVerify bool) {
	if opts.TLSConfig == nil {
		// Operator used LENS_REDIS_TLS=true with a redis:// URL.
		// Create a fresh TLS config; TLS 1.2 minimum is consistent with
		// the rest of the stack (Postgres, HTTPS server).
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	// InsecureSkipVerify is an explicit setter — false is also intentional
	// (it clears any pre-existing skip-verify that ParseURL may have set).
	opts.TLSConfig.InsecureSkipVerify = skipVerify
}
