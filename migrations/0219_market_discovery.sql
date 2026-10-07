-- B32.50 — discovery: trending by distinct buyers, search by capability and price, public collections.
--
-- market_capabilities is the controlled list of what a listing can do, kept as data: a capability is added here, never
-- typed free-hand on a listing. listing_capabilities says which of them a listing declares; search narrows by one.
CREATE TABLE IF NOT EXISTS market_capabilities (
    slug       TEXT PRIMARY KEY CHECK (slug ~ '^[a-z][a-z0-9-]{0,39}$'),
    label      TEXT NOT NULL,
    position   INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO market_capabilities (slug, label, position) VALUES
    ('summarize', 'Summarize', 10),
    ('extract', 'Extract', 20),
    ('translate', 'Translate', 30),
    ('classify', 'Classify', 40),
    ('code-review', 'Code review', 50),
    ('sql', 'SQL', 60),
    ('legal', 'Legal', 70),
    ('finance', 'Finance', 80),
    ('write', 'Write', 90),
    ('research', 'Research', 100),
    ('data-analysis', 'Data analysis', 110),
    ('customer-support', 'Customer support', 120)
ON CONFLICT (slug) DO NOTHING;

CREATE TABLE IF NOT EXISTS listing_capabilities (
    listing_id TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    capability TEXT NOT NULL REFERENCES market_capabilities (slug),
    PRIMARY KEY (listing_id, capability)
);
CREATE INDEX IF NOT EXISTS idx_listing_capabilities_capability ON listing_capabilities (capability, listing_id);

-- Search reads a public listing's title and description as English text.
CREATE INDEX IF NOT EXISTS idx_market_listings_search ON market_listings
    USING gin (to_tsvector('english', title || ' ' || description)) WHERE visibility = 'public';

-- The nightly job's figures: per listing and UTC day, the uses that ran, the distinct buyers and the revenue (µUSD of
-- billed uses and licences, refunds out). A use by the seller itself, or by a workspace linked to the seller (a shared
-- card or owner — asked when the job runs, not only when the use was charged), is not counted.
CREATE TABLE IF NOT EXISTS market_listing_stats (
    listing_id         TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    day                DATE NOT NULL,
    uses               INTEGER NOT NULL CHECK (uses >= 0),
    distinct_buyers    INTEGER NOT NULL CHECK (distinct_buyers >= 0),
    revenue_usd_micros BIGINT NOT NULL CHECK (revenue_usd_micros >= 0),
    computed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (listing_id, day)
);

-- A listing's trending score: each distinct buyer of the last seven days counted once, weighted down by the days since
-- its latest use. A listing nobody bought in the window has no row.
CREATE TABLE IF NOT EXISTS market_listing_trending (
    listing_id         TEXT PRIMARY KEY REFERENCES market_listings (id) ON DELETE CASCADE,
    score              DOUBLE PRECISION NOT NULL CHECK (score >= 0),
    distinct_buyers_7d INTEGER NOT NULL CHECK (distinct_buyers_7d >= 0),
    computed_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A collection is a workspace's titled list of listings. A public one is listed for everyone; the operator marks
-- Talyvor's own featured, and featured ones are listed first. Only a public collection may be featured.
CREATE TABLE IF NOT EXISTS market_collections (
    id           TEXT PRIMARY KEY,                -- col_<uuid>
    workspace_id TEXT NOT NULL,
    title        TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 120),
    description  TEXT NOT NULL DEFAULT '',
    public       BOOLEAN NOT NULL DEFAULT false,
    featured     BOOLEAN NOT NULL DEFAULT false,
    featured_by  TEXT NOT NULL DEFAULT '',
    featured_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (public OR NOT featured)
);
CREATE INDEX IF NOT EXISTS idx_market_collections_workspace ON market_collections (workspace_id);
CREATE INDEX IF NOT EXISTS idx_market_collections_public ON market_collections (featured DESC, updated_at DESC) WHERE public;

CREATE TABLE IF NOT EXISTS market_collection_items (
    collection_id TEXT NOT NULL REFERENCES market_collections (id) ON DELETE CASCADE,
    listing_id    TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    position      INTEGER NOT NULL,
    PRIMARY KEY (collection_id, listing_id),
    UNIQUE (collection_id, position)
);
