-- 0137_workspace_compression_retired.sql — B18.6: the prompt rewriter is retired, for every workspace.
--
-- The rewriter (internal/compressor, switched per workspace by compression_policy, 0117) measured
-- 0.000% saved over 308 committed corpus prompts while rewriting 8 of 8 agent-traffic ones (unfenced
-- code loses its indentation), and it collapsed a conversation into one user message. Tare replaces it.
-- PUT /v1/workspaces/{ws}/compression now answers 410 for anything but 'disabled' and registration
-- stores 'disabled' whatever it is sent, so after this migration no workspace can have it on.
--
-- EXISTING ROWS: every workspace whose policy is not 'disabled' is set to 'disabled'. The NOTICE
-- prints how many, so the deploy log says exactly how many workspaces changed.

DO $$
DECLARE
  n BIGINT;
BEGIN
  UPDATE workspaces SET compression_policy = 'disabled', updated_at = NOW() WHERE compression_policy <> 'disabled';
  GET DIAGNOSTICS n = ROW_COUNT;
  RAISE NOTICE '0137: compression_policy set to disabled on % workspace(s)', n;
END $$;
