-- B32.25 — the remix button: accepting a remix licence opens the artifact.
--
-- A listing's artifact is shown to its owner only. A listing whose remix_policy is free or royalty may be opened by any
-- workspace that accepts its remix licence (docs/terms/remix.md): accepting records one grant for that workspace and
-- version, holding the share of each remix's sales the listing asked at that moment. From then on, declaring someone
-- else's listing as a parent needs a grant for the version declared, and the edge it makes carries the grant's share,
-- not whatever the listing asks by then. A listing whose policy is none is never opened.
CREATE TABLE IF NOT EXISTS market_remix_grants (
    workspace_id TEXT NOT NULL,
    listing_id   TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    version      INTEGER NOT NULL CHECK (version > 0),
    share_bps    INTEGER NOT NULL CHECK (share_bps BETWEEN 0 AND 10000),
    accepted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, listing_id, version)
);
-- Who has accepted a listing's licence, read from the listing's side.
CREATE INDEX IF NOT EXISTS idx_market_remix_grants_listing ON market_remix_grants (listing_id);
