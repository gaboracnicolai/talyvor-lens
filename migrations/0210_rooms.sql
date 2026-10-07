-- B32.28 — rooms: open chats where people and their agents build a product together.
--
-- A room belongs to the workspace that created it, which is its owner. Joining accepts the room's terms at their
-- current version: how a sale of the room's work is split, the share a fork pays its original, the default price of a
-- contribution and who may spend the room's budget. When the owner changes the terms they get a new version, and each
-- member is asked again before their next contribution (B32.31) — their row keeps the version they accepted.
-- Membership is per workspace; a member's agents join only as that member's. Removing a member stamps removed_at and
-- keeps the row, so what they contributed stays theirs.
CREATE TABLE IF NOT EXISTS rooms (
    id                 TEXT PRIMARY KEY,                -- room_…
    owner_workspace_id TEXT NOT NULL,
    title              TEXT NOT NULL,
    topic              TEXT NOT NULL DEFAULT '',
    description        TEXT NOT NULL DEFAULT '',
    visibility         TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public', 'private')),
    status             TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'locked', 'closed')),
    terms_version      INTEGER NOT NULL DEFAULT 1 CHECK (terms_version > 0),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_activity_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_rooms_owner ON rooms (owner_workspace_id);
-- GET /v1/rooms: the open public rooms, latest activity first.
CREATE INDEX IF NOT EXISTS idx_rooms_open_public ON rooms (last_activity_at DESC) WHERE visibility = 'public' AND status = 'open';

CREATE TABLE IF NOT EXISTS room_terms (
    room_id                  TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    version                  INTEGER NOT NULL CHECK (version > 0),
    split_rule               TEXT NOT NULL CHECK (split_rule IN ('owner_decides', 'equal', 'by_votes')),
    remix_share_bps          INTEGER NOT NULL CHECK (remix_share_bps BETWEEN 0 AND 10000), -- at most LENS_LINEAGE_MAX_SHARE_BPS
    default_price_usd_micros BIGINT NOT NULL CHECK (default_price_usd_micros >= 0),
    spend_policy             TEXT NOT NULL CHECK (spend_policy IN ('owner_only', 'members_with_spend')),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, version)
);

CREATE TABLE IF NOT EXISTS room_members (
    room_id       TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    workspace_id  TEXT NOT NULL,
    user_id       TEXT NOT NULL DEFAULT '',             -- who joined for the workspace
    role          TEXT NOT NULL CHECK (role IN ('owner', 'editor', 'member', 'viewer')),
    may_spend     BOOLEAN NOT NULL DEFAULT false,
    terms_version INTEGER NOT NULL CHECK (terms_version > 0),
    joined_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    removed_at    TIMESTAMPTZ,
    PRIMARY KEY (room_id, workspace_id),
    CHECK (role <> 'viewer' OR NOT may_spend)           -- a viewer is never given the room's budget
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_room_members_one_owner ON room_members (room_id) WHERE role = 'owner';
-- "the rooms you are in".
CREATE INDEX IF NOT EXISTS idx_room_members_workspace ON room_members (workspace_id) WHERE removed_at IS NULL;

CREATE TABLE IF NOT EXISTS room_member_agents (
    room_id      TEXT NOT NULL,
    agent_id     TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    joined_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, agent_id),
    FOREIGN KEY (room_id, workspace_id) REFERENCES room_members (room_id, workspace_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_room_member_agents_workspace ON room_member_agents (workspace_id);
