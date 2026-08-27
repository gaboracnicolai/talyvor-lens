package main

import (
	"net/http"

	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/keypool"
)

// operator_reads_handlers.go — the two OPERATOR reads that any tenant can call,
// extracted so what they put on the wire is provable.
//
// ⚠ NEITHER OF THESE IS FIXED HERE, AND THAT IS DELIBERATE. Both are registered
// inside `authed` with no further gate, and both have a MUTATING sibling on the
// same path that IS requireAdmin:
//
//	GET    /v1/api/keys/pool             — no gate      ← this file
//	POST   /v1/api/keys/pool             requireAdmin
//	DELETE /v1/api/keys/pool/{keyID}     requireAdmin
//	GET    /v1/api/fallback/chains       — no gate      ← this file
//	PUT    /v1/api/fallback/chains/{provider}  requireAdmin
//
// The writes are gated and the reads are not. Closing that means making a read
// requireAdmin, which changes WHO MAY CALL a route — a decision about the product,
// not a repair, and not a session's to take. W6.18 records it for Nicolai with the
// measurement attached; unlike W6.15's case there is no undeclared field to remove,
// because neither route appears in internal/api/openapi.go at all.
//
// What IS a session's to do is make the surface provable and put a tripwire on the
// one thing that would turn an operator-telemetry read into a credential leak: see
// operator_reads_handlers_test.go, which seeds a real-looking provider key into the
// pool and asserts the response cannot contain it. keypool.PoolKey holds the actual
// provider key in its `Key` field and carries NO json tags, so anything that ever
// marshals PoolKey rather than KeyStats would put every provider credential on this
// unauthenticated-to-a-tenant route.

// keyPoolStatter is the slice of *keypool.Pool this route needs.
type keyPoolStatter interface {
	Stats() []keypool.KeyStats
}

// fallbackChainLister is the slice of *fallback.FallbackRouter this route needs.
type fallbackChainLister interface {
	AllChains() map[string][]fallback.FallbackTarget
}

func newKeyPoolStatsHandler(p keyPoolStatter) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONOK(w, http.StatusOK, p.Stats())
	}
}

func newFallbackChainsHandler(r fallbackChainLister) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONOK(w, http.StatusOK, r.AllChains())
	}
}
