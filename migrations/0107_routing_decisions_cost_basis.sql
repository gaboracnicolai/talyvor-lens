-- 0107_routing_decisions_cost_basis.sql
-- Label which PRICING BASIS produced each routing_decisions row's two cost figures.
--
-- WHY THIS COLUMN EXISTS. Until now both figures were priced with alerts.CostUSD, which charges EVERY input
-- token at the full input rate — blind to prompt caching. Providers discount cached input heavily (Anthropic
-- bills a cache read at 0.10x input, OpenAI ~0.5x for the GPT-4o generation), and the customer gets that
-- discount free, with or without Lens in the path. So every row captured that way measured its "saving"
-- against a price nobody would have paid, inflating the delta by exactly the discount the customer already
-- had. Capture is default-on, so such rows exist on every deployment that has auto-routed anything.
--
-- ⚠ THE OLD ROWS CANNOT BE REPRICED, and this is the whole reason for a label rather than a backfill. To
-- reprice them you need the cached/uncached SPLIT of their input tokens. That split was never stored — the
-- table has input_tokens and output_tokens and nothing else — and it is not derivable from anything kept.
-- The provider's usage breakdown was read at serve time, used, and discarded. So the honest options were:
-- delete the history (destroys evidence), leave it silently mixed in (a basis change mid-series is its own
-- false signal — the exact failure this substrate is meant to replace), or MARK it. This marks it.
--
-- DEFAULT 'flat' is therefore not a placeholder, it is the CORRECT value for every pre-existing row: they
-- were priced flat. New cache-aware rows write 'cache_aware' explicitly. Rows where the provider reported no
-- usage breakdown also stay 'flat' — with nothing to split, flat is the only basis available, and such a row
-- must not masquerade as measured.
--
-- The reader (routedecision.Summarize) sums ONLY 'cache_aware' rows and returns the count of excluded flat
-- rows alongside, so the discontinuity is visible in the readout instead of buried in an average.
--
-- Additive and idempotent: one column with a constant default (metadata-only on PG 11+, no table rewrite),
-- plus a partial index for the aggregate's hot filter. No data is moved and no existing reader changes shape.
ALTER TABLE routing_decisions
    ADD COLUMN IF NOT EXISTS cost_basis TEXT NOT NULL DEFAULT 'flat';

-- The aggregate filters on cost_basis = 'cache_aware' within a (workspace_id, created_at) window. The
-- existing idx_routing_decisions_ws already serves the window; this partial index keeps the basis filter
-- from degrading into a heap re-check as flat history accumulates and is never read again.
CREATE INDEX IF NOT EXISTS idx_routing_decisions_ws_cache_aware
    ON routing_decisions (workspace_id, created_at DESC)
    WHERE cost_basis = 'cache_aware';
