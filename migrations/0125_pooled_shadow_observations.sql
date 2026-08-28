-- 0125_pooled_shadow_observations.sql — the WRITE-SIDE shadow log for cross-tenant cache pooling.
--
-- WHAT IT IS FOR. W4.9 asks for a log of what WOULD have pooled, so the pooled hit rate becomes
-- measurable on real traffic while pooling stays OFF for consumers. Every prior measurement of that
-- rate (cmd/hitrate, poolsafety.MeasureRephrase) runs over COMMITTED FIXTURES whose own header calls
-- them harder than reality. This table is the first real-traffic substrate for the same question.
--
-- ⚠ WHY IT IS WRITTEN FROM THE WRITE SIDE, WHICH IS THE WHOLE DESIGN AND NOT AN IMPLEMENTATION
-- DETAIL. The obvious shadow log — on a private miss, perform the pooled LOOKUP anyway and record
-- the would-be hit without serving it — CAN ONLY EVER RECORD ZERO. Both pooled read surfaces
-- (proxy.tryExactPooled, proxy.trySemanticPooled) read a keyspace that is written ONLY under
-- cache_pooling.PoolabilityGate.DecidePoolableOnWrite, and BOTH sides route through the same
-- Participant predicate (global switch AND workspace opt-in). With pooling off the pooled keyspace
-- is empty, so a lookup-based shadow log reports "0 would have pooled" forever, and the next reader
-- concludes the pool has no value. The number would be structurally zero and would read as measured.
--
-- So a row is written when a fresh, cacheable, non-PII response comes back from a provider — the
-- exact population whose REPEATS would have been pool hits — and the hit rate is computed offline
-- from repeats of the fingerprint. Nothing is read from the pooled keyspace at any point.
--
-- WHAT IS STORED, AND WHAT DELIBERATELY IS NOT. No prompt text and no response bytes: only SHA-256
-- fingerprints of key material. A fingerprint cannot be un-hashed into a prompt, so this table
-- cannot become a prompt log by accident, and a cross-tenant repeat is detectable without either
-- tenant's text ever sitting in one row. It carries no cost, no token counts and no ledger units:
-- it is an observation, not a financial fact, and nothing downstream can aggregate it into money.
--
-- HOW IT IS READ BACK. poolshadow.Recorder.Rate(ctx, since, ttl) — and the ttl argument is the
-- POINT: a pooled Redis entry lives for cfg.MaxCacheTTL, so a repeat that arrives after the entry
-- expired is NOT a hit the pool could have served. The caller passes the configured TTL rather than
-- this file naming a number nobody measured. In SQL, directly:
--
--   -- would-be CROSS-TENANT exact pool hits in the last 24h, at a 24h entry TTL
--   SELECT count(*) FROM pooled_shadow_observations o
--    WHERE o.observed_at >= now() - interval '24 hours'
--      AND EXISTS (SELECT 1 FROM pooled_shadow_observations e
--                   WHERE e.pooled_key_fp = o.pooled_key_fp
--                     AND e.workspace_id <> o.workspace_id
--                     AND e.observed_at < o.observed_at
--                     AND e.observed_at >= o.observed_at - interval '24 hours');
--
-- ⚠ THE `workspace_id <> ` IS LOAD-BEARING AND IS NOT A DETAIL. A repeat from the SAME workspace
-- would have been served by the workspace-PRIVATE cache, which is already on. Counting it would
-- credit the pool with savings the private cache already delivers — the single easiest way to make
-- this table overstate the thing it exists to measure.
--
-- ⚠ AND canon_fp IS A CEILING, NOT A HIT. cmd/hitrate measured that the semantic pool is bounded by
-- semanticSelectPooledSQL's `discriminators = $6` EXACT ENTITY EQUALITY, not by the cosine
-- threshold, and that this bound is threshold-independent. canon_fp fingerprints exactly that
-- canonical form, so a cross-tenant repeat of it is a pair the entity gate WOULD have admitted —
-- a NECESSARY condition for a semantic serve, never a sufficient one. Similarity is not recorded
-- here and cannot be inferred from here. Anyone quoting a canon_fp count as a hit rate is quoting
-- an upper bound as a measurement.
--
-- INERT BY DEFAULT: nothing writes this table unless LENS_POOL_SHADOW_LOG_ENABLED is set.

CREATE TABLE IF NOT EXISTS pooled_shadow_observations (
    id             BIGSERIAL PRIMARY KEY,
    observed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    workspace_id   TEXT        NOT NULL,
    provider       TEXT        NOT NULL,
    model          TEXT        NOT NULL,
    -- SHA-256 over the same key material proxy.pooledPromptKey feeds the pooled exact cache,
    -- namespaced by provider and model so an entry can never be attributed across either.
    pooled_key_fp  BYTEA       NOT NULL,
    -- SHA-256 over discriminator.Canon(prompt), namespaced identically. NULL when the canonical
    -- form is empty (Canonical.Verifiable() == false) — such a prompt is unservable from the
    -- semantic pool in BOTH directions, so it has no ceiling to contribute to.
    canon_fp       BYTEA,
    -- Whether the pooled copy was ACTUALLY written (the gate was on). False is the shadow case.
    -- Recorded rather than assumed so a reader can tell a shadow row from a live one without
    -- knowing what the flag was set to on the day.
    did_pool       BOOLEAN     NOT NULL DEFAULT false
);

-- The read-back's self-join is (fingerprint, time) on both sides.
CREATE INDEX IF NOT EXISTS pooled_shadow_observations_key_at_idx
    ON pooled_shadow_observations (pooled_key_fp, observed_at);
CREATE INDEX IF NOT EXISTS pooled_shadow_observations_canon_at_idx
    ON pooled_shadow_observations (canon_fp, observed_at) WHERE canon_fp IS NOT NULL;
CREATE INDEX IF NOT EXISTS pooled_shadow_observations_at_idx
    ON pooled_shadow_observations (observed_at);

COMMENT ON TABLE pooled_shadow_observations IS
    'Write-side shadow log for cross-tenant cache pooling: one row per fresh cacheable upstream response. Fingerprints only, no prompt text. Read back via poolshadow.Recorder.Rate. A lookup-based shadow log would be structurally zero — see the header of migrations/0125_pooled_shadow_observations.sql.';
COMMENT ON COLUMN pooled_shadow_observations.canon_fp IS
    'Entity-gate CEILING, not a hit: a cross-tenant repeat is a pair semanticSelectPooledSQL''s discriminators equality would admit. Similarity is not recorded and cannot be inferred.';
