-- B32.31 — contributions: a room's members propose work, fork each other's with lineage, and vote.
--
-- A contribution is a marketplace listing published with visibility 'room': its owner and the live members of its room
-- see it (and open its artifact), everyone else gets 404, and the catalog never lists it. room_id names the room and is
-- set exactly when the visibility is 'room'. It is not a foreign key: a room deleted with its owner's workspace leaves
-- each member's listing to that member alone, as hidden() reads a room that has no members.
ALTER TABLE market_listings ADD COLUMN IF NOT EXISTS room_id TEXT;
ALTER TABLE market_listings DROP CONSTRAINT IF EXISTS market_listings_visibility_check;
ALTER TABLE market_listings ADD CONSTRAINT market_listings_visibility_check
    CHECK (visibility IN ('public', 'unlisted', 'private', 'room'));
ALTER TABLE market_listings DROP CONSTRAINT IF EXISTS market_listings_room_check;
ALTER TABLE market_listings ADD CONSTRAINT market_listings_room_check CHECK ((visibility = 'room') = (room_id IS NOT NULL));
CREATE INDEX IF NOT EXISTS idx_market_listings_room ON market_listings (room_id) WHERE room_id IS NOT NULL;

-- One row per listing version proposed to a room. forked_from is the contribution a fork was made from; the lineage
-- edge it pays (source room_fork, at the room's remix share) is in market_lineage. The owner or an editor accepts or
-- rejects a contribution; decided_by_workspace_id and decided_at say who and when.
CREATE TABLE IF NOT EXISTS room_contributions (
    id                      TEXT PRIMARY KEY,                -- rcon_…
    room_id                 TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    listing_id              TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    version                 INTEGER NOT NULL CHECK (version > 0),
    author_workspace_id     TEXT NOT NULL,
    author_user_id          TEXT NOT NULL DEFAULT '',
    forked_from             TEXT REFERENCES room_contributions (id) ON DELETE SET NULL,
    status                  TEXT NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'accepted', 'rejected')),
    decided_by_workspace_id TEXT NOT NULL DEFAULT '',
    decided_at              TIMESTAMPTZ,
    message_id              TEXT NOT NULL,                   -- the room message that announced it
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (listing_id, version)
);
-- A room's contributions, newest first.
CREATE INDEX IF NOT EXISTS idx_room_contributions_room ON room_contributions (room_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_room_contributions_author ON room_contributions (author_workspace_id);

-- A member's vote on a contribution: +1 or -1, one per member, replaced by its next. The tally is their sum.
CREATE TABLE IF NOT EXISTS room_votes (
    contribution_id TEXT NOT NULL REFERENCES room_contributions (id) ON DELETE CASCADE,
    workspace_id    TEXT NOT NULL,
    value           SMALLINT NOT NULL CHECK (value IN (-1, 1)),
    voted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (contribution_id, workspace_id)
);
CREATE INDEX IF NOT EXISTS idx_room_votes_workspace ON room_votes (workspace_id);
