-- 0124_mark_prompt_hash_writerless.sql — THE FOURTH WRITERLESS COLUMN, AND THE CORRECTION OF
-- 0114'S CLAIM THAT A FOURTH COULD NOT ARRIVE UNNOTICED.
--
-- Migration 0114 documented token_events.cached, .compressed and .savings_pct as columns nothing
-- writes, and ended with: "internal/catalog/writerless_column_guard_test.go fails the build if a
-- fourth appears." THAT SENTENCE WAS NOT TRUE, and this migration exists because a fourth
-- appeared and the build stayed green.
--
--   token_events.prompt_hash has existed since 0001_init.sql (NOT NULL DEFAULT '') and has NEVER
--   had a writer. Censused, not assumed: the only two production statements that write this table
--   are internal/alerts insertTokenEventSQL and insertCacheServeSQL, and neither names it. Every
--   row carries the empty string, forever.
--
-- WHY 0114'S GUARD COULD NOT SEE IT — two independent reasons, both now fixed:
--
--   1. Its writerlessColumns list is HAND-ENUMERATED. It re-checks three columns someone already
--      wrote down; it cannot discover a fourth. The claim was about a capability it never had.
--   2. Its matcher covers FILTER / WHERE / AND / SUM / AVG. The two real uses of prompt_hash are
--      GROUP BY and JOIN ... ON — neither is in that set.
--
-- WHAT IT COST, measured on a real database over the full migration chain: internal/learner
-- analyseSQL grouped by this column, so nine spend rows covering THREE distinct prompts collapsed
-- into ONE group and Analyse returned a single insight with an empty pattern and a hit count equal
-- to every non-cache-served request in the window. Two customer-facing surfaces rendered it and
-- multiplied it into estimated_monthly_savings_usd: GET /v1/api/models/recommendations and the MCP
-- tool get_model_recommendations. Fixed in 439dca9.
--
-- STILL OPEN AND DELIBERATELY NOT FIXED HERE: internal/warmer candidatesSQL JOINs token_events on
-- this column, so the cache warmer has never had a candidate — measured with a positive control (0
-- candidates; insert one row carrying a real hash and the same query returns 1). Repairing it is a
-- privacy decision (a per-prompt fingerprint retained under the DEFAULT `metadata` logging policy,
-- whose stated purpose is to strip prompt identity) AND a money decision (the prompt_text that
-- join recovers is itself empty under that same policy, and the warmer sends what it recovers to a
-- paid provider on the operator's key). Recorded, not guessed.
--
-- ⚠ WHY THIS COMMENTS RATHER THAN DROPS — 0114's three reasons apply unchanged: token_events is a
-- partitioned hot table (0034), so DROP COLUMN takes ACCESS EXCLUSIVE on the parent and every
-- partition; this repo has no down-migrations, so a drop is one-way; and altering the billing
-- table's shape has a different risk profile from correcting a query. prompt_hash belongs with the
-- other three in the change that eventually drops them, with a measured lock window.
--
-- Until then this is what a person running \d+ token_events sees.

COMMENT ON COLUMN token_events.prompt_hash IS
  'WRITERLESS — always the empty string. No INSERT or UPDATE has ever set this column; the only '
  'two writers are internal/alerts insertTokenEventSQL and insertCacheServeSQL and neither names '
  'it. Do NOT GROUP BY it, JOIN on it, or predicate on it: every row is identical, so a grouping '
  'collapses to one row and a join to prompt_embeddings.prompt_hash (which IS written, a sha256) '
  'matches nothing. Both mistakes have shipped. Slated for removal; see migration 0124.';
