-- 0129_token_events_auth_method.sql — B9.3: which credential made each request.
--
-- B9.1 found four of production's six pooled serves uncharged with no way to tell what sent them.
-- Every token_events row now records the credential kind that made the request (auth.Method*:
-- session_key for the browser chat, workspace_key, jwt, global_key, …). '' = not recorded: every row
-- written before this column existed, and any write that reached the writer with no credential.
--
-- Additive, own file, no row rewritten. ADD COLUMN on the partitioned parent reaches every partition.

ALTER TABLE token_events
    ADD COLUMN IF NOT EXISTS auth_method TEXT NOT NULL DEFAULT '';
