-- B32.24 — remix terms and the family tree: a listing version declares its parents.
--
-- A listing says whether it may be remixed: none (the default), free, or royalty with the share of each sale its remixes
-- send up to it (remix_share_bps, 1 to LENS_LINEAGE_MAX_SHARE_BPS). A version that builds on other listings declares
-- them as its parents, and each declaration is one market_lineage edge holding the parent's share as it stood then: a
-- later change to the parent's terms never touches an edge already made. Edges are never rewritten or removed.
ALTER TABLE market_listings ADD COLUMN IF NOT EXISTS remix_policy TEXT NOT NULL DEFAULT 'none'
    CHECK (remix_policy IN ('none', 'free', 'royalty'));
ALTER TABLE market_listings ADD COLUMN IF NOT EXISTS remix_share_bps INTEGER NOT NULL DEFAULT 0;
ALTER TABLE market_listings DROP CONSTRAINT IF EXISTS market_listings_remix_share_check;
ALTER TABLE market_listings ADD CONSTRAINT market_listings_remix_share_check
    CHECK ((remix_policy = 'royalty' AND remix_share_bps BETWEEN 1 AND 10000) OR (remix_policy <> 'royalty' AND remix_share_bps = 0));

CREATE TABLE IF NOT EXISTS market_lineage (
    id                TEXT PRIMARY KEY,
    child_listing_id  TEXT NOT NULL REFERENCES market_listings (id),
    child_version     INTEGER NOT NULL CHECK (child_version > 0),
    parent_listing_id TEXT NOT NULL REFERENCES market_listings (id),
    parent_version    INTEGER NOT NULL CHECK (parent_version > 0),
    share_bps         INTEGER NOT NULL CHECK (share_bps BETWEEN 0 AND 10000),
    source            TEXT NOT NULL CHECK (source IN ('declared', 'room_fork', 'claim')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (child_listing_id <> parent_listing_id),
    UNIQUE (child_listing_id, child_version, parent_listing_id)
);
-- The descendants of a listing, walked from parent to child.
CREATE INDEX IF NOT EXISTS idx_market_lineage_parent ON market_lineage (parent_listing_id);

-- The family tree is append-only: an edge, once declared, is what every later sale is paid by.
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON market_lineage
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_lineage
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
