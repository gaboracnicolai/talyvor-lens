-- B32.29 — private rooms: an invite link, or being named by the owner.
--
-- A private room is listed to its members only, and a workspace joins it through an invite. A link invite is an
-- unguessable token, kept here only as its SHA-256, with an expiry and a use count; it admits workspaces until it is
-- revoked, expires or is used up, and then answers 404. A named invite is the owner naming one workspace: that
-- workspace sees the room, reads its terms and joins once. Each join through an invite that makes a new member uses it.
CREATE TABLE IF NOT EXISTS room_invites (
    id                      TEXT PRIMARY KEY,           -- rinv_…
    room_id                 TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    workspace_id            TEXT NOT NULL DEFAULT '',   -- a named invite's workspace; '' for a link
    token_sha256            TEXT,                       -- a link's token, hashed; NULL for a named invite
    max_uses                INTEGER NOT NULL CHECK (max_uses > 0),
    uses                    INTEGER NOT NULL DEFAULT 0 CHECK (uses >= 0 AND uses <= max_uses),
    expires_at              TIMESTAMPTZ,                -- NULL: a named invite, which lasts until it is used or revoked
    revoked_at              TIMESTAMPTZ,
    created_by_workspace_id TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((workspace_id = '') = (token_sha256 IS NOT NULL)),
    CHECK (token_sha256 IS NULL OR expires_at IS NOT NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_room_invites_token ON room_invites (token_sha256) WHERE token_sha256 IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_room_invites_room ON room_invites (room_id, created_at DESC);
-- "the rooms you are invited to".
CREATE INDEX IF NOT EXISTS idx_room_invites_named ON room_invites (workspace_id) WHERE workspace_id <> '' AND revoked_at IS NULL;
