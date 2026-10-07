-- B32.46 — a similarity check before publishing catches undeclared copies.
--
-- Every version a publish adds is fingerprinted from its text — its title, its description and the strings of its
-- artifact: an embedding (internal/embedder; embedding_model names the space it lives in, so vectors of two models are
-- never compared) and a MinHash of its five-word shingles. A publish compares its fingerprint with those of the approved
-- public listings and of the contributions of the rooms its publisher belongs to; a match at or above
-- LENS_MARKET_SIMILARITY_HOLD to a listing that is neither a declared parent (nor an ancestor of one) nor the publisher's
-- own holds the version for review, naming the nearest listing and its score.
--
-- embedding is NULL when the embedder failed or the text has no words; the MinHash still compares. minhash is NULL when
-- the text has no words at all.
CREATE TABLE IF NOT EXISTS market_listing_fingerprints (
    listing_id      TEXT NOT NULL,
    version         INTEGER NOT NULL,
    embedding_model TEXT NOT NULL DEFAULT '',
    embedding       vector,
    minhash         BIGINT[],
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (listing_id, version),
    FOREIGN KEY (listing_id, version) REFERENCES market_listing_versions (listing_id, version) ON DELETE CASCADE
);
