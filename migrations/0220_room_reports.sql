-- B32.52 — reporting a room or one of its messages; a room's owner or editor mutes or bans a member; the operator locks or
-- closes a room.
--
-- A workspace reports a room, or one message in it (message_id; '' is the room itself), for one of the marketplace's
-- report reasons or harassment or spam. While a workspace's report of one thing is open it has one: reporting it again
-- changes nothing. A public room with LENS_ROOM_REPORTS_HIDE open reports from different workspaces leaves the public
-- list of rooms until the operator reviews it; reviewing resolves every open report of the room and its messages, as
-- kept, locked or closed (internal/rooms/reports.go). Each operator action is a row in operator_audit.
CREATE TABLE IF NOT EXISTS room_reports (
    id                    TEXT PRIMARY KEY,                  -- rrpt_…
    room_id               TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
    message_id            TEXT NOT NULL DEFAULT '',          -- '' reports the room itself
    reporter_workspace_id TEXT NOT NULL,
    reason                TEXT NOT NULL CHECK (reason IN ('malicious', 'injection', 'secret', 'personal_data', 'infringing',
                                                           'misleading', 'harassment', 'spam', 'other')),
    details               TEXT NOT NULL DEFAULT '' CHECK (char_length(details) <= 2000),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at           TIMESTAMPTZ,
    resolution            TEXT CHECK (resolution IN ('kept', 'locked', 'closed')),
    resolved_by           TEXT NOT NULL DEFAULT '',          -- the operator who reviewed it
    CHECK ((resolved_at IS NULL) = (resolution IS NULL))
);
-- One open report per reporter of one room or message.
CREATE UNIQUE INDEX IF NOT EXISTS uq_room_reports_open ON room_reports (room_id, message_id, reporter_workspace_id)
    WHERE resolved_at IS NULL;
-- The open reports of a room: the public list's count, and the operator's queue.
CREATE INDEX IF NOT EXISTS idx_room_reports_open ON room_reports (room_id) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_room_reports_reporter ON room_reports (reporter_workspace_id);

-- A muted member reads the room and posts nothing in it until it is unmuted; a banned one is removed from the room and
-- does not join it again until it is unbanned.
ALTER TABLE room_members ADD COLUMN IF NOT EXISTS muted_at TIMESTAMPTZ;
ALTER TABLE room_members ADD COLUMN IF NOT EXISTS banned_at TIMESTAMPTZ;
