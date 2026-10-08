-- B30.5 — Know Your Agent: a signed credential every agent can show (internal/kya, docs/kya.md).
--
-- Each row is one credential Lens issued: a JWT signed with Ed25519 holding the agent, its owner's verified name and
-- level, its capabilities and a summary of its limits. kid and public_key are the key that signed it — the JWKS at
-- /.well-known/talyvor-kya/jwks.json publishes every key with a credential not yet expired, so a credential stays
-- verifiable across a key change until it expires. claims_digest is the SHA-256 of what the credential says, less its
-- times and id: an agent whose facts no longer match is given a new one and the old one is revoked.
--
-- revoked_at and revoked_reason are the revocation list, published at /.well-known/talyvor-kya/revoked.json: freezing
-- or archiving an agent, or changing its rules, revokes its credential in the same transaction.

CREATE TABLE IF NOT EXISTS kya_credentials (
    id             TEXT PRIMARY KEY CHECK (id <> ''),
    workspace_id   TEXT NOT NULL CHECK (workspace_id <> ''),
    agent_id       TEXT NOT NULL CHECK (agent_id <> ''),
    kid            TEXT NOT NULL CHECK (kid <> ''),
    public_key     TEXT NOT NULL CHECK (public_key <> ''),
    claims_digest  TEXT NOT NULL CHECK (claims_digest <> ''),
    token          TEXT NOT NULL CHECK (token <> ''),
    issued_at      TIMESTAMPTZ NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL CHECK (expires_at > issued_at),
    revoked_at     TIMESTAMPTZ,
    revoked_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_kya_credentials_agent ON kya_credentials (agent_id, issued_at DESC);
CREATE INDEX IF NOT EXISTS idx_kya_credentials_workspace ON kya_credentials (workspace_id);
CREATE INDEX IF NOT EXISTS idx_kya_credentials_revoked ON kya_credentials (expires_at) WHERE revoked_at IS NOT NULL;

-- A credential deleted with its workspace before it expires stays on the revocation list: kya_revocation_tombstones
-- keeps its id, why and when until it would have expired — nothing of the workspace, the agent or its owner.
CREATE TABLE IF NOT EXISTS kya_revocation_tombstones (
    id         TEXT PRIMARY KEY CHECK (id <> ''),
    reason     TEXT NOT NULL CHECK (reason <> ''),
    revoked_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE OR REPLACE FUNCTION kya_credential_tombstone() RETURNS trigger AS $$
BEGIN
    IF OLD.expires_at > now() THEN
        INSERT INTO kya_revocation_tombstones (id, reason, revoked_at, expires_at)
        VALUES (OLD.id, CASE WHEN OLD.revoked_at IS NULL THEN 'deleted' ELSE OLD.revoked_reason END,
                COALESCE(OLD.revoked_at, now()), OLD.expires_at)
        ON CONFLICT (id) DO NOTHING;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE TRIGGER kya_credential_tombstone BEFORE DELETE ON kya_credentials
    FOR EACH ROW EXECUTE FUNCTION kya_credential_tombstone();
