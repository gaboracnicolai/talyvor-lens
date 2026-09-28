-- B17.1 — synthetic workspaces: test accounts that can never touch real money or real users.
--
-- A synthetic workspace is created and reset only through the operator-key routes
-- (POST /v1/synthetic/workspaces[/reset], registered only when LENS_SYNTHETIC_KEY is set).
-- It holds test credits only (an admin grant — no purchase, no conversion, no royalty), and its
-- pooled answers live in a partition of their own: shared among synthetic workspaces, never with
-- a real one in either direction. Nothing else ever sets the flag, and nothing clears it.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS synthetic BOOLEAN NOT NULL DEFAULT false;

-- Every call to the synthetic routes, refused ones included: what was asked, from where, and what
-- happened.
CREATE TABLE IF NOT EXISTS synthetic_operations (
    id          BIGSERIAL PRIMARY KEY,
    action      TEXT NOT NULL,                 -- create | reset
    accounts    INT NOT NULL DEFAULT 0,        -- how many workspaces were created or reset
    remote_addr TEXT NOT NULL DEFAULT '',
    outcome     TEXT NOT NULL,                 -- ok | unauthorized | rate_limited | error: …
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
