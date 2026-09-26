-- 0134_workspace_tare_distill_default_always.sql — B8.3: Tare and document conversion are ON by
-- default, for new AND existing workspaces.
--
-- Nicolai decided every capability is on by default and the customer turns off what they do not
-- want, from the Features screen (B8.2), which has a working switch for both of these.
--
--   tare_policy    — 0126 created it at DEFAULT 'disabled'. Now 'always': Tare reduces the newest
--                    message of every request unless that request carries X-Talyvor-Tare: false,
--                    and every reducer still refuses (sends the message unchanged) when unsure.
--   distill_policy — 0039 created it at DEFAULT 'disabled', while the application default
--                    (workspace.DefaultDistillPolicy) has been 'always' since distill was switched
--                    on by default. The catalog default now agrees with it.
--
-- EXISTING ROWS: every workspace whose policy reads 'disabled' is moved to 'always'. The column is
-- NOT NULL with a default, so a workspace that never chose is indistinguishable from one that chose
-- 'disabled' — nothing recorded who set it. The NOTICE below prints how many rows each UPDATE
-- touched, so the deploy log says exactly how many workspaces changed.
--
-- NOT TOUCHED HERE: pooling (cache_poolable, distill_poolable — B9.1/B9.2 own their safety) and
-- cost_optimize_routing (0104: an explicitly named model is never downgraded without the tenant's
-- consent). Registration no longer rewrites either policy on an existing workspace
-- (workspace.RegisterWorkspace), so an "off" set after this migration stays off.

ALTER TABLE workspaces
  ALTER COLUMN tare_policy SET DEFAULT 'always',
  ALTER COLUMN distill_policy SET DEFAULT 'always';

DO $$
DECLARE
  n_tare    BIGINT;
  n_distill BIGINT;
BEGIN
  UPDATE workspaces SET tare_policy = 'always', updated_at = NOW() WHERE tare_policy = 'disabled';
  GET DIAGNOSTICS n_tare = ROW_COUNT;
  UPDATE workspaces SET distill_policy = 'always', updated_at = NOW() WHERE distill_policy = 'disabled';
  GET DIAGNOSTICS n_distill = ROW_COUNT;
  RAISE NOTICE '0134: tare_policy disabled -> always on % workspace(s); distill_policy disabled -> always on % workspace(s)',
    n_tare, n_distill;
END $$;
