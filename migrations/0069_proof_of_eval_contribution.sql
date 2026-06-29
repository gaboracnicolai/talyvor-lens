-- 0069_proof_of_eval_contribution.sql — Proof-of-Improvement instance 1: proof-of-eval-contribution.
--
-- Rewards a contributor for adding a FAIR, VALIDATED, DISCRIMINATING eval question to the verifier
-- pool, paid on the item's measured discriminating value (NOT submission count) through the SAME
-- held-ledger / U6 chokepoint as every other mint. Author identity is added to the (previously
-- ownerless, 0068) eval pool so the author can be PERMANENTLY EXCLUDED from drawing / grading / being
-- paid on their own items. The author column is verifier-private and NEVER enters a node payload
-- (BuildProbeRequest reads only item.input).

-- (1) Author attribution + exact-dedup + a validation lifecycle on the eval pool.
ALTER TABLE benchmark_eval_items
    ADD COLUMN IF NOT EXISTS author_workspace_id TEXT,                       -- NULL = operator-seeded (ownerless); set = contributed
    ADD COLUMN IF NOT EXISTS content_hash        TEXT,                       -- hex(sha256(input)) — exact-dedup key (mirrors distill.ContentHash)
    ADD COLUMN IF NOT EXISTS status              TEXT NOT NULL DEFAULT 'active'; -- pending | active | quarantined

-- Exact-dedup: at most one item per content_hash. PARTIAL (NULL allowed) so legacy 0068 rows (no hash)
-- are untouched; a contributed duplicate is rejected at the seed/contribute write.
CREATE UNIQUE INDEX IF NOT EXISTS idx_eval_items_content_hash
    ON benchmark_eval_items (content_hash) WHERE content_hash IS NOT NULL;

-- Author index for the draw-time exclusion predicate + the mint scan.
CREATE INDEX IF NOT EXISTS idx_eval_items_author
    ON benchmark_eval_items (author_workspace_id) WHERE author_workspace_id IS NOT NULL;

-- (2) The once-per-item mint claim — mirrors pool_royalty_mints / distill_royalty_mints so the generic
--     FinalizeSweeper settles it unchanged. request_id IS the item_id (the once-per-item idempotency
--     key); contributor_workspace_id is the author; minted_amount/status/finalize_after are the columns
--     the finalize kernel reads. discrimination + distinct_graders are append-only audit of WHAT was paid.
CREATE TABLE IF NOT EXISTS eval_contribution_mints (
    request_id               TEXT PRIMARY KEY,            -- = benchmark_eval_items.id (mint ONCE per item)
    contributor_workspace_id TEXT             NOT NULL,   -- the author being paid
    discrimination           DOUBLE PRECISION NOT NULL,   -- the held-score paid on: clamp01(4·Var) over distinct unlinked graders
    distinct_graders         INTEGER          NOT NULL,   -- |G| at mint time (≥ MinUnlinkedGraders) — audit
    minted_amount            DOUBLE PRECISION NOT NULL,   -- rate × discrimination (what CreditHeldTx credited)
    status                   TEXT             NOT NULL DEFAULT 'held',
    finalize_after           TIMESTAMPTZ      NOT NULL,
    created_at               TIMESTAMPTZ      NOT NULL DEFAULT NOW()
);

-- The partial index the FinalizeSweeper SELECT rides (held rows past their finalize time).
CREATE INDEX IF NOT EXISTS idx_eval_contribution_mints_finalize
    ON eval_contribution_mints (finalize_after) WHERE status = 'held';
