// Package dbrouting is the single place the optional read-replica pool is
// distributed (U8/U9 read routing).
//
// THE INVARIANT (enforced by WIRING, not vigilance): money, authz, and any
// read inside a write transaction are NEVER handed the replica pool. Only the
// recon-confirmed analytics/display readers are constructed with
// ReadPool(primary, replica); every money/authz/tx consumer keeps the primary
// pool it has today. A misroute is made impossible by which pool each
// constructor physically receives — there is no runtime "route reads to
// replica" switch that could send a revoked-key lookup or a balance gate to a
// lagging replica.
package dbrouting

import "github.com/jackc/pgx/v5/pgxpool"

// ReadPool returns the pool analytics/display reads should use: the replica
// when one is configured, otherwise the primary. This is the off-by-default
// switch — a nil replica (LENS_DB_REPLICA_URL unset, malformed, or unreachable
// at boot → main.go leaves it nil) transparently yields the primary, so every
// read is byte-identical to single-pool behavior.
//
// ReadPool must ONLY be called for the replica-safe set (forecast, ROI,
// dashboard/usage stats, admin read-only lists, rate/history, display-only
// balance snapshots). Money, authz, and any read inside a write transaction
// MUST take the primary pool directly — never route those through here.
func ReadPool(primary, replica *pgxpool.Pool) *pgxpool.Pool {
	if replica != nil {
		return replica
	}
	return primary
}
