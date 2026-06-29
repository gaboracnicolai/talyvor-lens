-- 0071_eval_item_cohort.sql — Proof-of-Improvement piece 3 (proof-of-routing-prediction), PR-2:
-- cohort-tag the held eval pool.
--
-- Recon R2 found eval items are UNCOHORTED, so a routing prediction's "cohort C" (PR-1) has no
-- resolvable slice to be tested against. This PR adds the SAME three cohort dimensions
-- routing_predictions (0070) uses — feature_category, input_token_range, complexity_bucket — so PR-3 can
-- resolve "the held items in cohort C" against a prediction's cohort. INERT METADATA: no mint, no
-- scoring, no change to the node-blind probe payload, no change to the #10 draw / #250 author-exclusion.
--
-- Tagging: input_token_range + complexity_bucket are DERIVED from the item's input by the SAME exported
-- serve-path functions (internal/cohort.DeriveInputCohort → mining.InputBucketFor / router.AnalyseComplexity
-- / worktier.ComplexityBucketFor); feature_category is DECLARED at seed (mirroring the serve-time
-- X-Talyvor-Feature header — it is not derived in either path). Nullable so untagged legacy 0068/0069 rows
-- stay NULL (simply not matchable by PR-3 until re-seeded/backfilled).

ALTER TABLE benchmark_eval_items
    ADD COLUMN IF NOT EXISTS feature_category  TEXT,  -- declared at seed (NULL = untagged); matches routing_predictions
    ADD COLUMN IF NOT EXISTS input_token_range TEXT,  -- derived: mining.InputBucketFor(len(input)/4)
    ADD COLUMN IF NOT EXISTS complexity_bucket TEXT;  -- derived: worktier.ComplexityBucketFor(router.AnalyseComplexity(input)); '' = untiered

-- Slice lookup for the future scorer (PR-3): the tagged held items in a cohort.
CREATE INDEX IF NOT EXISTS idx_eval_items_cohort
    ON benchmark_eval_items (feature_category, input_token_range, complexity_bucket)
    WHERE feature_category IS NOT NULL;
