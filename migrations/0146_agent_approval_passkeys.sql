-- B19.16 — approvals signed with a passkey, and a web push when one is filed.
--
-- A workspace's owner registers passkeys (WebAuthn, "none" attestation: the credential's public key and
-- its signature counter). Once a workspace has one, approving or denying an agent's request takes a fresh
-- assertion from one of them over a single-use server challenge naming that approval. Each device the
-- owner wants told subscribes to web push; filing an approval sends every subscription of the workspace
-- one encrypted push (RFC 8291) naming the agent, the amount and the reason.
CREATE TABLE IF NOT EXISTS workspace_passkeys (
    credential_id TEXT PRIMARY KEY,           -- base64url, as the authenticator gave it
    workspace_id  TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    public_key    BYTEA NOT NULL,             -- SubjectPublicKeyInfo DER, ES256 (P-256)
    sign_count    BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_workspace_passkeys_workspace ON workspace_passkeys (workspace_id);

CREATE TABLE IF NOT EXISTS webauthn_challenges (
    challenge    TEXT PRIMARY KEY,            -- base64url of 32 random bytes
    workspace_id TEXT NOT NULL,
    purpose      TEXT NOT NULL,               -- 'register' | 'approval:<approval id>'
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS workspace_push_subscriptions (
    endpoint     TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    p256dh       TEXT NOT NULL,               -- base64url, the device's P-256 public key
    auth         TEXT NOT NULL,               -- base64url, 16 bytes
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_workspace_push_subscriptions_workspace ON workspace_push_subscriptions (workspace_id);
