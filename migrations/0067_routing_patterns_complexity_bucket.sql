-- 0067_routing_patterns_complexity_bucket.sql
-- WorkTier-Advisor tier-conditioned cohorts (Shape 3). The routing Advisor ranks
-- quality-per-dollar over cohorts of routing_patterns (the quality corpus). To
-- condition the pick on the request's COMPLEXITY tier, the complexity bucket must
-- ride on the quality row — routing_patterns and work_tier_observations share NO
-- join key, so a JOIN is impossible. This adds the bucket here, written at
-- pattern-capture time from the SAME router.AnalyseComplexity score the routing
-- decision and the persisted WorkTier use (worktier.ComplexityBucketFor).
--
-- ADDITIVE + metadata-only on PG: a constant DEFAULT '' is a catalog change with
-- NO table rewrite, no backfill, reversible. Legacy rows keep '' and are EXCLUDED
-- from the tiered aggregate (aggregateCohortsTieredSQL: complexity_bucket <> ''),
-- so the tiered corpus re-accumulates as new captures write real buckets — a
-- warm-up window of more fallback, which is safe. The non-tiered aggregate
-- (aggregateCohortsSQL) is UNCHANGED and ignores this column entirely.
--
-- PRIVACY: the tiered aggregate still enforces the per-cohort MinSamples /
-- MinWorkspaces floors via the per-TIERED-cohort COUNT(DISTINCT workspace_id), so a
-- finer slice with < MinWorkspaces distinct workspaces never surfaces.

ALTER TABLE routing_patterns
    ADD COLUMN IF NOT EXISTS complexity_bucket TEXT NOT NULL DEFAULT '';
