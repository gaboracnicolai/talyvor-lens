// Package ha provides optional High-Availability coordination for Lens:
// instance registry + heartbeat, a Redis-backed shared rate limiter,
// cross-instance circuit-breaker gossip, and drain-aware health endpoints.
//
// HA is strictly opt-in via LENS_HA_ENABLED. When disabled (the default),
// every type in this package degrades to a safe no-op / in-process fallback
// so a single instance behaves exactly as it did before HA existed. Redis is
// never made a *new* hard requirement by this package — when HA is enabled
// but Redis is unavailable, the components fail open to local behaviour rather
// than failing the request path.
package ha
