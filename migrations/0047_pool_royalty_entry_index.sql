-- 0047_pool_royalty_entry_index.sql — Phase-2 Stage 2.3b follow-up: the
-- per-ENTRY mint cap's hot-path index.
--
-- The per-entry cap closes the per-pair ≠ per-entry gap: semantic ownership
-- churn (the pooled prompt_embeddings row's contributor_workspace_id is
-- overwritten on re-contribution) lets one entry_id accrue mints under
-- different contributor identities over a window, so the per-pair cap alone
-- doesn't bound an entry's total exposure. The cap adds a second COUNT in the
-- mint tx: `WHERE entry_id = $1 AND created_at > now() - window`.
--
-- That COUNT is HOT-PATH — it runs on every served mint when the cap is
-- enabled — and entry_id was previously UNINDEXED (the existing indexes are
-- on requester / contributor / the partial finalize set). Without this index
-- every mint would sequentially scan pool_royalty_mints. REQUIRED, not
-- optional: this is the analytical-can-scan vs. hot-path-must-be-indexed
-- line (the 2.3b detectors are background and may scan; this cannot).
--
-- Additive, idempotent, own-file (the 0043 idiom). The index also serves the
-- volume detector's per-entry grouping as a free side benefit.

CREATE INDEX IF NOT EXISTS idx_pool_royalty_mints_entry
    ON pool_royalty_mints (entry_id, created_at);
