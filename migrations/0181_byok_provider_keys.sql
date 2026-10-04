-- 0181_byok_provider_keys.sql — B27.26: BYOK, the one subscription tier.
--
-- A workspace on the BYOK plan stores its own provider keys and its requests go upstream on
-- them, with no token charge from Talyvor. Two things make that possible:
--
--   subscriptions.byok        — the live subscription is the BYOK plan (Stripe lookup key
--                               talyvor_byok_monthly), set by the webhook from the Price it billed.
--   workspace_provider_keys   — one sealed key per (workspace, provider), envelope-encrypted by
--                               internal/envelope under LENS_PROVIDER_SECRET_KEK. Every column but
--                               last4 is ciphertext, a nonce or a key id; there is no plaintext column.
--
-- key_id is indexed because the KEK rotation runbook (docs/provider-secret-envelope.md, step 4)
-- asks which rows still name an old key.

ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS byok BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS workspace_provider_keys (
    workspace_id TEXT NOT NULL,
    provider     TEXT NOT NULL,
    key_id       TEXT NOT NULL,
    wrapped_dek  BYTEA NOT NULL,
    dek_nonce    BYTEA NOT NULL,
    ciphertext   BYTEA NOT NULL,
    ct_nonce     BYTEA NOT NULL,
    -- The last four characters, the only part of the key ever shown back.
    last4        TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, provider)
);

CREATE INDEX IF NOT EXISTS idx_workspace_provider_keys_key_id ON workspace_provider_keys (key_id);
