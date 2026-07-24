-- 0104_workspace_cost_optimize_routing.sql — per-tenant CONSENT to cost-optimised
-- routing on concrete (explicitly named) models.
--
--   cost_optimize_routing — when TRUE, this workspace has delegated model choice
--                           to Lens on concrete requests too: the router MAY
--                           downgrade an explicitly named model to a cheaper one
--                           in the same provider family when the prompt is judged
--                           simple. When FALSE (the default), an explicitly named
--                           model is HONOURED exactly — never downgraded — because
--                           the customer chose it and quality is theirs to decide.
--                           The "auto" pseudo-model and the X-Talyvor-Auto-Route
--                           header are unaffected: those are per-request delegation
--                           and route regardless of this flag.
--
-- Defaults to FALSE: every existing and new workspace has its explicit model
-- honoured until an admin opts it in (PUT /v1/workspaces/{wsID}/cost-optimize-routing).
-- The founder's rule — quality shall not be compromised without consent — is the
-- default; the saving is available only where consented. Additive, idempotent
-- (ADD COLUMN IF NOT EXISTS), no row rewrite — lands on the same workspaces table
-- that already holds cache_poolable (0041) and distill_policy (0039).

ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS cost_optimize_routing BOOLEAN NOT NULL DEFAULT false;
