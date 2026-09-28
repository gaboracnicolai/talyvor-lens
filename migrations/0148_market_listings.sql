-- B20.1 — publish: a listing is a versioned agent, prompt, skill, evaluation or pipeline.
--
-- (Not marketplace_listings: that is 0024's retired LENS token exchange, B18.1.)
--
-- A listing belongs to the workspace that publishes it and carries its price per use (µLXC; 0 is free)
-- and its visibility. Its artifact lives in versions: publishing a change adds a version and never
-- rewrites one, so whatever was built on an earlier version keeps working. Every version records what
-- the publish scan found (internal/market/scan.go).
CREATE TABLE IF NOT EXISTS market_listings (
    id                 TEXT PRIMARY KEY,
    workspace_id       TEXT NOT NULL,
    kind               TEXT NOT NULL CHECK (kind IN ('agent', 'prompt', 'skill', 'evaluation', 'pipeline')),
    title              TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    price_per_use_ulxc BIGINT NOT NULL DEFAULT 0 CHECK (price_per_use_ulxc >= 0),
    visibility         TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public', 'unlisted', 'private')),
    latest_version     INTEGER NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_market_listings_workspace ON market_listings (workspace_id);
CREATE INDEX IF NOT EXISTS idx_market_listings_public ON market_listings (kind, created_at DESC) WHERE visibility = 'public';

CREATE TABLE IF NOT EXISTS market_listing_versions (
    listing_id      TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    version         INTEGER NOT NULL CHECK (version > 0),
    artifact        JSONB NOT NULL,
    artifact_sha256 TEXT NOT NULL,
    changelog       TEXT NOT NULL DEFAULT '',
    scan            JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (listing_id, version)
);

-- A version is never rewritten: what a buyer used stays what it was.
CREATE OR REPLACE TRIGGER market_listing_versions_immutable BEFORE UPDATE ON market_listing_versions
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
