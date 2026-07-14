-- 0091_routing_decisions.sql
-- KEEL ECONOMY step 1 — DESCRIPTIVE, MINT-FREE evidence of the routing Advisor's cost impact. This is the
-- go/no-go substrate: it measures how often cross-tenant cohort intelligence OVERRODE the pre-cohort default,
-- and the estimated cost delta — the data that decides whether a corpus-contribution mint (KE-1) is worth
-- building at all. It moves NO money.
--
-- TENANCY: workspace_id is SELF only — there is DELIBERATELY no counterparty column.
--
-- ⚠ THE ESTIMATE (read this before anyone builds a mint on it): counterfactual_cost_estimate_u is the cost of
-- baseline_model priced AT THE ACTUAL served token counts. A different model emits a different number of
-- tokens, so this is a COUNTERFACTUAL ESTIMATE, never a measured fact. It is EVIDENCE, not mint-backing. There
-- is deliberately NO "saving" column so this table can never be mistaken for money; any future mint MUST use a
-- CONSERVATIVE, house-favouring, floored-at-zero formulation (pay strictly less than the estimate), never a
-- naive (counterfactual − actual) delta of these columns. Costs are integer µ-USD (SEC-2 discipline: no float
-- in a stored amount).
CREATE TABLE IF NOT EXISTS routing_decisions (
    id                             BIGSERIAL PRIMARY KEY,
    workspace_id                   TEXT NOT NULL,               -- SELF only — no counterparty column
    baseline_model                 TEXT NOT NULL,               -- the pre-cohort default (router.Route)
    actual_model                   TEXT NOT NULL,               -- the model actually served
    cohort_overrode                BOOLEAN NOT NULL,            -- did cross-tenant cohort intelligence override the baseline?
    cohort_basis                   TEXT NOT NULL DEFAULT '',    -- the recommendation basis (dims); '' when none
    cohort_n                       INTEGER NOT NULL DEFAULT 0,  -- distinct workspaces in the cohort that informed the rec
    input_tokens                   INTEGER NOT NULL,
    output_tokens                  INTEGER NOT NULL,
    actual_cost_u                  BIGINT NOT NULL,             -- µ-USD of the served model (integer)
    counterfactual_cost_estimate_u BIGINT NOT NULL,            -- ⚠ ESTIMATE (see header): baseline_model priced at ACTUAL tokens; NOT money
    created_at                     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_routing_decisions_ws ON routing_decisions (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_routing_decisions_window ON routing_decisions (created_at);
