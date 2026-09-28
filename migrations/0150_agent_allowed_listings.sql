-- B19.14 — an agent may use only the marketplace listings its rules allow.
--
-- Empty (the default) allows every listing, as before. Once it names any, the agent may use those listings
-- and no other; it does not restrict the agent's own model calls, which allowed_models and
-- allowed_providers judge.
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS allowed_listings TEXT[] NOT NULL DEFAULT '{}';
