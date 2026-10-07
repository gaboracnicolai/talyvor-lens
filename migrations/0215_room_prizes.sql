-- B32.35 — room prizes, paid as a purchase of the winning contribution.
--
-- A room's owner posts a prize — a title, what wins it, its amount and its deadline — no larger than what the room
-- wallet's monthly limit has left. Awarding it, the owner buys the winning contribution: one billed market_uses row of
-- use_kind prize at the prize's amount, the owner's workspace the buyer and the room's wallet the agent, and a perpetual
-- commercial licence to the version that won (market_licences kind prize, source prize). Nothing is escrowed and
-- nothing moves between owners: the contribution's author earns their share when the owner's invoice clears. A prize
-- not awarded by its deadline closes and charges nothing.
CREATE TABLE IF NOT EXISTS room_prizes (
    id                  TEXT PRIMARY KEY,                -- rprz_…
    room_id             TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    poster_workspace_id TEXT NOT NULL,                   -- the room's owner
    poster_user_id      TEXT NOT NULL DEFAULT '',
    title               TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 120),
    criteria            TEXT NOT NULL DEFAULT '' CHECK (char_length(criteria) <= 4000),
    amount_usd_micros   BIGINT NOT NULL CHECK (amount_usd_micros > 0),
    deadline            TIMESTAMPTZ NOT NULL,
    status              TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'awarded', 'closed')),
    contribution_id     TEXT REFERENCES room_contributions (id) ON DELETE SET NULL, -- the winner
    winner_workspace_id TEXT NOT NULL DEFAULT '',
    use_id              TEXT,                            -- the billed prize row
    licence_id          TEXT,                            -- the owner's licence to what won
    awarded_at          TIMESTAMPTZ,
    closed_at           TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (deadline > created_at),
    CHECK ((status = 'awarded') = (use_id IS NOT NULL AND licence_id IS NOT NULL AND awarded_at IS NOT NULL)),
    CHECK ((status = 'closed') = (closed_at IS NOT NULL))
);
-- A room's prizes, newest first.
CREATE INDEX IF NOT EXISTS idx_room_prizes_room ON room_prizes (room_id, created_at DESC);
-- The open prizes past their deadline, which the sweep closes.
CREATE INDEX IF NOT EXISTS idx_room_prizes_open ON room_prizes (deadline) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_room_prizes_poster ON room_prizes (poster_workspace_id);

-- A licence won as a room's prize says so.
ALTER TABLE market_licences DROP CONSTRAINT IF EXISTS market_licences_source_check;
ALTER TABLE market_licences ADD CONSTRAINT market_licences_source_check CHECK (source IN ('offer', 'rent_to_own', 'prize'));
