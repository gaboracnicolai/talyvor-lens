-- B32.30 — a room's messages, and the log of what happened to them that its members' streams read.
--
-- Room text is stored by Talyvor and shown to the room's members, or to everyone in a public room — unlike a private
-- chat, which stays in the browser. In a public room every message is scanned as a listing is published (a secret or
-- personal data refuses it) and the row keeps what the scan found; a private room's messages are not scanned (scan is
-- NULL). Editing keeps the row and stamps edited_at; deleting empties the body and stamps deleted_at, so a tombstone
-- stays where the message was.
--
-- seq is the order a room's messages are paged in, and room_events.cursor the order its events are streamed in. Both
-- are taken while the writer holds the room's row lock (internal/rooms/messages.go), so within one room they commit in
-- the order they were taken and a reader that has seen cursor N never later finds a smaller one appear.
CREATE TABLE IF NOT EXISTS room_messages (
    id                      TEXT PRIMARY KEY,                -- rmsg_…
    seq                     BIGSERIAL NOT NULL UNIQUE,
    room_id                 TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    author_workspace_id     TEXT NOT NULL,
    author_user_id          TEXT NOT NULL DEFAULT '',
    author_agent_id         TEXT NOT NULL DEFAULT '',
    kind                    TEXT NOT NULL CHECK (kind IN ('text', 'contribution', 'run', 'prize', 'system')),
    body                    TEXT NOT NULL DEFAULT '' CHECK (char_length(body) <= 8000),
    refs                    JSONB NOT NULL DEFAULT '{}',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    edited_at               TIMESTAMPTZ,
    deleted_at              TIMESTAMPTZ,
    deleted_by_workspace_id TEXT NOT NULL DEFAULT '',        -- the author, or the owner or editor who removed it
    scan                    JSONB,                           -- a public room's message: what the publish scan found
    CHECK (deleted_at IS NULL OR body = '')
);
-- A page of a room's messages, latest first.
CREATE INDEX IF NOT EXISTS idx_room_messages_room ON room_messages (room_id, seq DESC);
-- A member's messages in the last minute (LENS_ROOM_MESSAGES_PER_MINUTE).
CREATE INDEX IF NOT EXISTS idx_room_messages_author ON room_messages (author_workspace_id, room_id, created_at DESC);

CREATE TABLE IF NOT EXISTS room_events (
    cursor  BIGSERIAL PRIMARY KEY,
    room_id TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    kind    TEXT NOT NULL CHECK (kind IN ('message.posted', 'message.edited', 'message.deleted')),
    ref     TEXT NOT NULL,                                   -- the message's id
    at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- GET /v1/rooms/{id}/events: a room's events after the client's cursor, once a second.
CREATE INDEX IF NOT EXISTS idx_room_events_room ON room_events (room_id, cursor);
