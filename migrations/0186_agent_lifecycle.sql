-- B28.298 — an agent can be renamed, described and archived.
--
-- description is what the agent is for, in its owner's words. archived_at marks an agent retired: archiving
-- sweeps its balance back to the workspace in one withdraw entry and revokes its keys in the same
-- transaction, and from then on it can neither be funded nor move money of its own.
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
