// Package cache_pooling holds the Phase-2 Stage 2.0 shared-cache governance
// gate. It decides whether a cross-tenant ("pooled") exact-cache entry may be
// written or served. It is strictly READ-ONLY: it consults the global switch
// and the per-workspace cache_poolable flag and mutates nothing — no cache
// writes, no DB writes, and crucially NO ledger writes (lxc_ledger /
// token_events). Pool-B royalty minting is a separate, gated build.
//
// Pooling is inert-by-default: every decision is false unless the global switch
// is on AND the relevant workspace(s) have explicitly opted in. The gate is
// dependency-injected via closures so this package imports neither config nor
// workspace (no import cycles) and is trivially testable.
package cache_pooling

import "context"

// PoolabilityGate answers poolability questions from a global-switch reader and
// a per-workspace opt-in reader. All methods are nil-safe: a nil gate (pooling
// not wired) reports false for everything.
type PoolabilityGate struct {
	enabled  func() bool            // global LENS_CACHE_POOLABLE_ENABLED
	poolable func(wsID string) bool // per-workspace cache_poolable
}

// New builds a gate. enabled reads the global switch; poolable reads a
// workspace's cache_poolable flag (false for unknown/unset workspaces).
func New(enabled func() bool, poolable func(wsID string) bool) *PoolabilityGate {
	return &PoolabilityGate{enabled: enabled, poolable: poolable}
}

// Participant reports whether wsID is an opted-in pooling participant: the
// global switch is on AND the workspace has cache_poolable=true. Cheap (an
// in-memory map read) so the request path can gate the pooled lookup before
// paying for any extra cache round-trip.
func (g *PoolabilityGate) Participant(wsID string) bool {
	if g == nil || g.enabled == nil || g.poolable == nil {
		return false
	}
	return g.enabled() && g.poolable(wsID)
}

// DecidePoolableOnWrite reports whether a contributor's fresh, cacheable entry
// should ALSO be written to the shared pool — i.e. the global switch is on and
// the contributing workspace opted in.
func (g *PoolabilityGate) DecidePoolableOnWrite(_ context.Context, contributorWsID string) bool {
	return g.Participant(contributorWsID)
}

// MaybeAllowPooledHit reports whether a pooled entry contributed by ownerWsID
// may be served to requesterWsID. It requires ALL THREE: the global switch on,
// the requester opted in, AND the contributor (owner) opted in. An empty owner
// — a pre-feature entry with no recorded provenance — is never poolable.
func (g *PoolabilityGate) MaybeAllowPooledHit(_ context.Context, requesterWsID, ownerWsID string) bool {
	if !g.Participant(requesterWsID) {
		return false
	}
	if ownerWsID == "" {
		return false
	}
	return g.poolable(ownerWsID)
}
