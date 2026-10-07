-- B32.32 — the room's budget: a room wallet with rules, and who may spend it.
--
-- Creating a room creates its wallet: an agent account in the owner's workspace, of kind 'room', with one proxy-scoped
-- key. The owner funds it and sets its rules as any agent's; its monthly limit is the room's budget, at most the
-- owner's plan's room_budget_max_usd (rooms_plan_limits). A room wallet does not count toward the agents a plan allows.
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'agent';
ALTER TABLE agent_accounts DROP CONSTRAINT IF EXISTS agent_accounts_kind_check;
ALTER TABLE agent_accounts ADD CONSTRAINT agent_accounts_kind_check CHECK (kind IN ('agent', 'room'));

-- The room's wallet. Set in the transaction that opens the room; NULL for a room opened before this migration, which
-- gets its wallet the next time its owner reads it.
ALTER TABLE rooms ADD COLUMN IF NOT EXISTS wallet_agent_id TEXT REFERENCES agent_accounts (id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_rooms_wallet ON rooms (wallet_agent_id) WHERE wallet_agent_id IS NOT NULL;

-- Every charge on a room records the room and the member who spent; '' on every other use.
ALTER TABLE market_uses ADD COLUMN IF NOT EXISTS room_id TEXT NOT NULL DEFAULT '';
ALTER TABLE market_uses ADD COLUMN IF NOT EXISTS actor_workspace_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_market_uses_room ON market_uses (room_id, used_at) WHERE room_id <> '';

-- What a posting was for, in words: a room wallet's postings name the room and the member who spent.
ALTER TABLE agent_postings ADD COLUMN IF NOT EXISTS memo TEXT;
