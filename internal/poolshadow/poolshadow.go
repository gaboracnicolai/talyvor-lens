// Package poolshadow persists what cross-tenant cache pooling WOULD have served, and computes the
// would-be hit rate from it — with pooling itself left off.
//
// ⚠ THE DESIGN CONSTRAINT THAT SHAPES THE WHOLE PACKAGE. The natural shadow log is "on a private
// miss, do the pooled lookup anyway and record the would-be hit without serving it". It can only
// ever record ZERO. Both pooled read surfaces (proxy.tryExactPooled, proxy.trySemanticPooled) read
// a keyspace written only under cache_pooling.PoolabilityGate.DecidePoolableOnWrite, and read and
// write route through the SAME Participant predicate. With pooling off the pooled keyspace is
// empty, so the lookup reports "nothing would have pooled" forever — a structural zero that reads
// as a measurement. So this package observes the WRITE side (a fresh, cacheable response that just
// cost money upstream) and derives hits from repeats, offline.
//
// It stores fingerprints, never prompt text or response bytes. See
// migrations/0125_pooled_shadow_observations.sql for the table and the read-back SQL.
package poolshadow

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/discriminator"
)

// Observation is one fresh, cacheable upstream response — the population whose cross-tenant
// repeats would have been pool hits.
type Observation struct {
	WorkspaceID string
	Provider    string
	Model       string
	// PooledKeyFP fingerprints the EXACT pooled key material. Built by Observe from the caller's
	// pooled key so this package never re-derives the key rule; production's rule stays the only one.
	PooledKeyFP []byte
	// CanonFP fingerprints discriminator.Canon(prompt). Nil when the canonical form is empty.
	CanonFP []byte
	// DidPool records whether the pooled copy was actually written (the gate was on).
	DidPool bool
	// At overrides the row timestamp. Zero means "now", which is what the serve path uses; tests
	// and backfills set it explicitly.
	At time.Time
}

// fingerprint hashes key material under a namespace so two different kinds of fingerprint can
// never be equal by accident, and so provider/model are inside the hash rather than beside it.
func fingerprint(kind, provider, model, material string) []byte {
	h := sha256.New()
	// Length-prefixed rather than delimiter-joined: "a"+"bc" and "ab"+"c" must not collide.
	for _, part := range []string{kind, provider, model, material} {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write([]byte(part))
	}
	return h.Sum(nil)
}

// Observe builds an Observation from the pooled key material the caller already computed.
//
// ⚠ pooledKey IS PASSED IN, NOT DERIVED HERE, ON PURPOSE. The proxy owns the pooled key rule
// (cache.PooledPromptKey). If this package re-implemented it, a change on one side would silently
// stop the shadow numbers corresponding to the pool they describe, and nothing would go red.
func Observe(workspaceID, provider, model, pooledKey, rawPrompt string, didPool bool) Observation {
	o := Observation{
		WorkspaceID: workspaceID,
		Provider:    provider,
		Model:       model,
		PooledKeyFP: fingerprint("exact", provider, model, pooledKey),
		DidPool:     didPool,
	}
	if canon := discriminator.Canon(rawPrompt); canon.Verifiable() {
		o.CanonFP = fingerprint("canon", provider, model, string(canon))
	}
	return o
}

const insertSQL = `
INSERT INTO pooled_shadow_observations
    (observed_at, workspace_id, provider, model, pooled_key_fp, canon_fp, did_pool)
VALUES (COALESCE($1, now()), $2, $3, $4, $5, $6, $7)`

// rateSQL computes both figures in ONE pass so they always describe the same population.
//
// A "would-be cross-tenant hit" is an observation with an EARLIER observation of the same
// fingerprint, from a DIFFERENT workspace, no older than the pooled entry's TTL. Each of those
// three conditions is load-bearing:
//
//	different workspace — a same-workspace repeat is a PRIVATE cache hit; pooling buys nothing.
//	earlier             — the first sighting is the contribution, not a hit.
//	within the TTL      — a pooled Redis entry that has expired cannot be served.
const rateSQL = `
SELECT
  count(*),
  count(*) FILTER (WHERE EXISTS (
      SELECT 1 FROM pooled_shadow_observations e
       WHERE e.pooled_key_fp = o.pooled_key_fp
         AND e.workspace_id <> o.workspace_id
         AND e.observed_at < o.observed_at
         AND e.observed_at >= o.observed_at - make_interval(secs => $2))),
  count(*) FILTER (WHERE o.canon_fp IS NOT NULL AND EXISTS (
      SELECT 1 FROM pooled_shadow_observations e
       WHERE e.canon_fp = o.canon_fp
         AND e.workspace_id <> o.workspace_id
         AND e.observed_at < o.observed_at
         AND e.observed_at >= o.observed_at - make_interval(secs => $2)))
FROM pooled_shadow_observations o
WHERE o.observed_at >= $1`

// Rate is the read-back. since bounds the reporting window; ttl is how long a pooled entry lives.
//
// ⚠ ttl IS AN ARGUMENT AND NOT A CONSTANT HERE. It is cfg.MaxCacheTTL — the TTL the exact cache is
// actually constructed with — so this package never names a duration nobody measured, and a
// deployment that changes the cache TTL changes this figure with it.
type Rates struct {
	// Observations is the denominator: fresh cacheable upstream responses in the window. A rate
	// quoted without it is a rate over an unknown population.
	Observations int64
	// CrossTenantExactHits is what byte-identical cross-tenant repeats would have served.
	CrossTenantExactHits int64
	// CrossTenantEntityGateCeiling is an UPPER BOUND on the semantic lane, never a hit count: it
	// counts pairs the SQL entity equality would admit, before any similarity test.
	CrossTenantEntityGateCeiling int64
}

// Recorder writes observations and reads the rate back. It holds a pool and nothing else — no
// cache client, so it cannot read or write the pooled keyspace even by mistake.
type Recorder struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Recorder { return &Recorder{pool: pool} }

// Record persists one observation.
func (r *Recorder) Record(ctx context.Context, o Observation) error {
	if r == nil || r.pool == nil {
		return errors.New("poolshadow: no pool")
	}
	if len(o.PooledKeyFP) == 0 {
		return errors.New("poolshadow: empty pooled key fingerprint")
	}
	var at any
	if !o.At.IsZero() {
		at = o.At
	}
	if _, err := r.pool.Exec(ctx, insertSQL,
		at, o.WorkspaceID, o.Provider, o.Model, o.PooledKeyFP, o.CanonFP, o.DidPool); err != nil {
		return fmt.Errorf("poolshadow: insert: %w", err)
	}
	return nil
}

// Rate computes the would-be hit figures over [since, now] at the given pooled-entry TTL.
func (r *Recorder) Rate(ctx context.Context, since time.Time, ttl time.Duration) (Rates, error) {
	var out Rates
	if r == nil || r.pool == nil {
		return out, errors.New("poolshadow: no pool")
	}
	row := r.pool.QueryRow(ctx, rateSQL, since, ttl.Seconds())
	if err := row.Scan(&out.Observations, &out.CrossTenantExactHits, &out.CrossTenantEntityGateCeiling); err != nil {
		return out, fmt.Errorf("poolshadow: rate: %w", err)
	}
	return out, nil
}
