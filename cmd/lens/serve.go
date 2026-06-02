package main

// serve.go — TLS-aware HTTP server startup + graceful shutdown.
//
// Two modes:
//
//   Plain HTTP (TLSDomain == "")
//     Binds on cfg.ListenAddr (default 0.0.0.0:8080).
//     Intended for local development and deployments where TLS is
//     terminated upstream (nginx, Cloudflare, load balancer).
//
//   HTTPS via Let's Encrypt (TLSDomain != "")
//     Binds on :443 with a certificate provisioned automatically by
//     Let's Encrypt.  A second server on :80 handles ACME HTTP-01
//     challenges and issues a 301 redirect for every other request.
//     Certificates are cached in TLSCacheDir across restarts.
//
// TLS configuration:
//   - Minimum version: TLS 1.2 (TLS 1.0 and 1.1 are broken and rejected).
//   - TLS 1.3 is used whenever the client supports it; Go's crypto/tls
//     manages TLS 1.3 cipher suites automatically — they cannot and should
//     not be overridden.
//   - TLS 1.2 is restricted to ECDHE + AEAD suites (GCM-SHA256/384 and
//     ChaCha20-Poly1305).  This excludes CBC, RC4, 3DES, and static RSA.
//   - Curve preferences: X25519 first (fastest + most secure), then P-256.

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/talyvor/lens/internal/config"
)

// serverSet holds the live server(s) so the caller can shut them all down.
type serverSet struct {
	main     *http.Server
	redirect *http.Server // non-nil in TLS mode only
}

// shutdown stops both servers. The redirect server is stateless (it only
// issues 301s) so it gets a short bounded context — capped at 5 s — rather
// than context.Background(). This ensures a slow port-80 connection cannot
// consume time from the main server's drain window (drainCtx).
func (s *serverSet) shutdown(drainCtx context.Context) error {
	if s.redirect != nil {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		_ = s.redirect.Shutdown(rctx)
	}
	return s.main.Shutdown(drainCtx)
}

// startServers builds and launches the correct server set for the given
// configuration, returning a handle for shutdown and a channel that receives
// at most one fatal startup error.
//
// The error channel is closed (without a value) on clean server exit
// (http.ErrServerClosed), so the caller can use it in a select alongside
// ctx.Done() without needing a nil-check on the read value.
func startServers(cfg *config.Config, handler http.Handler, logger *slog.Logger) (*serverSet, <-chan error) {
	if cfg.TLSDomain == "" {
		return startPlainHTTP(cfg, handler, logger)
	}
	return startTLS(cfg, handler, logger)
}

// ── plain HTTP ────────────────────────────────────────────────────────────────

func startPlainHTTP(cfg *config.Config, handler http.Handler, logger *slog.Logger) (*serverSet, <-chan error) {
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening (HTTP — set LENS_TLS_DOMAIN to enable HTTPS)",
			slog.String("addr", cfg.ListenAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	return &serverSet{main: srv}, errCh
}

// ── TLS / Let's Encrypt ───────────────────────────────────────────────────────

func startTLS(cfg *config.Config, handler http.Handler, logger *slog.Logger) (*serverSet, <-chan error) {
	certManager := &autocert.Manager{
		// AcceptTOS must be set — Let's Encrypt requires accepting their ToS.
		Prompt: autocert.AcceptTOS,

		// HostWhitelist restricts cert provisioning to the configured domain.
		// Without this, autocert would accept any domain, making it possible
		// for an attacker to trigger cert requests for arbitrary hostnames and
		// burn your Let's Encrypt rate-limit quota.
		HostPolicy: autocert.HostWhitelist(cfg.TLSDomain),

		// DirCache persists the certificate and private key to disk so a
		// restart doesn't trigger a new ACME round-trip.  The directory must
		// be writable by the Lens process and should be backed by durable
		// storage (not a tmpfs/ramdisk).
		Cache: autocert.DirCache(cfg.TLSCacheDir),
	}

	tlsCfg := &tls.Config{
		// GetCertificate resolves the Let's Encrypt certificate at TLS
		// handshake time, serving the cached cert or triggering ACME if none
		// is cached yet.
		GetCertificate: certManager.GetCertificate,

		// Reject TLS 1.0 and 1.1 — both are deprecated (RFC 8996) and have
		// known protocol-level weaknesses (BEAST, POODLE, etc.).
		MinVersion: tls.VersionTLS12,

		// Prefer X25519 (fastest, 128-bit equivalent security) then P-256.
		// This controls the key-agreement step of the TLS handshake.
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
		},

		// Restrict TLS 1.2 to ECDHE + AEAD suites only.
		// TLS 1.3 cipher suites are managed automatically by Go's crypto/tls
		// and are not configurable here — they are always strong.
		// CBC-mode suites are excluded (BEAST/Lucky-13 mitigations).
		// RSA key-exchange suites are excluded (no forward secrecy).
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
	}

	mainSrv := &http.Server{
		Addr:              ":443",
		Handler:           hstsMiddleware(handler),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Port 80 serves two purposes:
	//   1. Let's Encrypt ACME HTTP-01 challenge (/.well-known/acme-challenge/…)
	//      — autocert.Manager.HTTPHandler handles this automatically.
	//   2. Permanent redirect for all other requests to the HTTPS equivalent.
	// The ACME challenge must remain reachable on port 80 even after initial
	// provisioning because Let's Encrypt renews certificates every 90 days.
	redirectSrv := &http.Server{
		Addr:              ":80",
		Handler:           certManager.HTTPHandler(http.HandlerFunc(httpsRedirectHandler)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("http redirect server listening", slog.String("addr", ":80"))
		if err := redirectSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Log but do not propagate — a port-80 failure is non-fatal when
			// the cert is already cached (ACME won't need it until renewal).
			logger.Warn("http redirect server error", slog.String("err", err.Error()))
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening (TLS / Let's Encrypt)",
			slog.String("addr", ":443"),
			slog.String("domain", cfg.TLSDomain),
			slog.String("cert_cache", cfg.TLSCacheDir),
		)
		// Empty cert/key paths: TLSConfig.GetCertificate supplies the cert.
		if err := mainSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	return &serverSet{main: mainSrv, redirect: redirectSrv}, errCh
}

// httpsRedirectHandler issues a 301 Moved Permanently redirect from the
// HTTP URL to its HTTPS equivalent, preserving the path and query string.
// The port is stripped from r.Host because a client that sends "Host: example.com:80"
// would otherwise be redirected to https://example.com:80/… (port 80 over TLS — wrong).
func httpsRedirectHandler(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	target := "https://" + host + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// hstsMiddleware wraps a handler and stamps Strict-Transport-Security on
// every response.  It is applied only to the TLS server (port 443) —
// never to the plain-HTTP redirect server — so browsers only pin the
// header over a connection they already know is secure.
//
// max-age=63072000 = 2 years (the recommended minimum for HSTS preloading).
// includeSubDomains covers any sub-domain served by the same instance.
// preload is intentionally omitted until the domain is submitted to the
// HSTS preload list (https://hstspreload.org) — it cannot be undone easily.
func hstsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}
