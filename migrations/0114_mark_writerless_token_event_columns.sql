-- 0114_mark_writerless_token_event_columns.sql — SAY IT WHERE THE NEXT READER LOOKS.
--
-- token_events.cached, .compressed and .savings_pct have existed since 0001_init.sql and have
-- NEVER had a writer. Not one INSERT names them; no UPDATE touches them. Every row therefore
-- carries the column default, forever.
--
-- That is not a harmless quirk — it has produced a wrong customer-facing number three separate
-- times, each written by someone who read the schema and reasonably assumed a column means
-- something:
--
--   internal/api/server.go    reported a structural 0 as a measured cache hit rate.
--                             Fixed by 0100 (serve_source), and documented in that file.
--   internal/mcp/server.go    the SAME defect, missed by that fix. get_cache_stats answered
--                             estimated_savings_usd = $0.00 on every deployment, because
--                             SUM(cost_usd) FILTER (WHERE NOT cached) is the total spend and
--                             COUNT(*) FILTER (WHERE cached) is zero. Fixed alongside this.
--   internal/learner/         `AND cached = false` — an INERT filter, true for every row, so
--                             already-cached prompts were returned as pre-warming candidates
--                             and inflated a savings estimate derived from their hit_count.
--                             Fixed alongside this.
--
-- ⚠ WHY THIS COMMENTS RATHER THAN DROPS. Dropping would make the trap unrepresentable, which is
-- normally the stronger move and is the one this codebase reaches for elsewhere. Three things
-- argue against doing it *here*:
--
--   1. token_events is a PARTITIONED HOT TABLE (0034). DROP COLUMN takes ACCESS EXCLUSIVE on the
--      parent and every partition. Migrations run as a gated pre-boot step, so this would not
--      stall live traffic — it would stall the DEPLOY, for a duration nobody has measured on a
--      production-sized table.
--   2. This repo has NO down-migrations (stated in the deploy runbook's rollback matrix). A drop
--      is one-way. Rolling back the query corrections that ship with it would leave the schema
--      changed and the code expecting the column gone.
--   3. Correcting a wrong number and altering the billing table's shape have very different risk
--      profiles, and bundling them means one cannot be reverted without the other.
--
-- The information argument for keeping is weak — every value is the default, so nothing would be
-- lost — which is why the recommendation is to DROP THESE THREE COLUMNS in a change of their own,
-- with a measured lock window, once the readers are gone. As of this migration they are: the
-- three fixes above removed the last of them, and
-- internal/catalog/writerless_column_guard_test.go fails the build if a fourth appears.
--
-- Until that change lands, this comment is what a person running `\d+ token_events` sees. The Go
-- guard protects this repository; this protects someone with a psql prompt and a reporting query.

COMMENT ON COLUMN token_events.cached IS
  'WRITERLESS — always false. No INSERT or UPDATE has ever set this column. Do NOT predicate on '
  'it: a cache hit is serve_source LIKE ''cache_hit%'' (migration 0100). Reading this column has '
  'produced a wrong customer-facing number three times. Slated for removal; see migration 0114.';

COMMENT ON COLUMN token_events.compressed IS
  'WRITERLESS — always false. No writer has ever existed. Slated for removal; see migration 0114.';

COMMENT ON COLUMN token_events.savings_pct IS
  'WRITERLESS — always 0. No writer has ever existed. Savings are derived from serve_source and '
  'cost_usd, never from this column. Slated for removal; see migration 0114.';
