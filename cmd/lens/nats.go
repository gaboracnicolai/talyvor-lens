package main

import (
	"crypto/tls"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/talyvor/lens/internal/config"
)

// connectNATS establishes the NATS connection with TLS applied when
// either the URL scheme is tls:// / nats+tls:// or LENS_NATS_TLS=true.
//
// The TLS posture mirrors the Redis and Postgres layers:
//   - tls:// or nats+tls:// URL → TLS already requested by the nats client;
//     we layer in InsecureSkipVerify when LENS_NATS_TLS_SKIP_VERIFY=true.
//   - LENS_NATS_TLS=true → forces TLS even when the URL is nats://; useful
//     when a secrets manager always emits a nats:// URL.
//   - neither → logs a startup warning; connection is plaintext.
//
// InsecureSkipVerify should only be used for self-signed / internal-CA
// certificates in controlled environments (ISO 27001 A.13).
func connectNATS(url string, cfg *config.Config, logger *slog.Logger) (*nats.Conn, error) {
	// TLS is active when either:
	//   (a) the URL uses tls:// or nats+tls:// — the nats client enables TLS
	//       automatically on these schemes, or
	//   (b) LENS_NATS_TLS=true — operator forces TLS on a nats:// URL.
	urlLower := strings.ToLower(url)
	schemeHasTLS := strings.HasPrefix(urlLower, "tls://") ||
		strings.HasPrefix(urlLower, "nats+tls://")
	natsTLSActive := schemeHasTLS || cfg.NatsTLS

	var opts []nats.Option
	if natsTLSActive {
		tlsCfg := &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.NatsTLSSkipVerify, //nolint:gosec // opt-in, controlled envs only
		}
		opts = append(opts, nats.Secure(tlsCfg))
		logger.Info("nats TLS enabled", slog.Bool("skip_verify", cfg.NatsTLSSkipVerify))
		if cfg.NatsTLSSkipVerify {
			logger.Warn("nats TLS certificate verification is disabled (LENS_NATS_TLS_SKIP_VERIFY=true)" +
				" — only appropriate for self-signed certs in controlled environments")
		}
	} else {
		logger.Warn("nats TLS is disabled — inter-service messages are unencrypted;" +
			" use a tls:// URL or set LENS_NATS_TLS=true for production")
	}

	return nats.Connect(url, opts...)
}
