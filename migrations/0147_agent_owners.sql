-- B19.11 — know your agent: every agent tied to a verified owner.
--
-- owner_user_id is the person who created the agent (the signed-in user's id), or who later claimed it.
-- An agent with none cannot hold a balance: funding it, topping it up or paying it is refused until a
-- person of its workspace claims it. Agents created before this migration have none.
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS owner_user_id TEXT NOT NULL DEFAULT '';
