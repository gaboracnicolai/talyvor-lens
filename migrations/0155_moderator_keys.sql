-- B20.13 — a moderator key for the marketplace review queue.
--
-- Nicolai decided (28 Sep 2026) the web app gets a MODERATOR key, not Lens admin: one scope,
-- marketplace moderation. It reads /v1/admin/marketplace/review, approves a listing, and takes one
-- down with a reason, and nothing else. Keys are created and revoked by an operator command
-- (`lens moderator-keys`), stored as a sha256 hash only, and every use is recorded below with the
-- operator the web app names.
CREATE TABLE IF NOT EXISTS moderator_keys (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    key_hash    TEXT NOT NULL UNIQUE,
    key_prefix  TEXT NOT NULL,            -- the first characters, so a listing can say which key is which
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ,              -- NULL while the key works; set once, by `lens moderator-keys revoke`
    revoked_by  TEXT NOT NULL DEFAULT ''
);

-- Every use of a moderator key, append-only like every other audit record (0055).
CREATE TABLE IF NOT EXISTS moderator_key_uses (
    id          BIGSERIAL PRIMARY KEY,
    key_id      BIGINT NOT NULL REFERENCES moderator_keys (id),
    operator    TEXT NOT NULL,            -- who the web app says is acting (X-Talyvor-Operator)
    method      TEXT NOT NULL,
    path        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_moderator_key_uses_key ON moderator_key_uses (key_id, created_at DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON moderator_key_uses
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON moderator_key_uses
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
