-- B32.49 — the trust panel: reviews from buyers who paid.
--
-- A buyer workspace that paid for a use or a licence of a listing (a billed market_uses row that ran, not refunded) and
-- is not linked to its seller (the single-party detector: a shared card or owner) may review it: one review per buyer
-- per listing, a rating of 1 to 5 and its text, which it may rewrite. The seller may reply to each. The trust read
-- (internal/market/trust.go) counts a review only while its writer still qualifies, so a buyer later found linked to the
-- seller drops out of the count and the average.
CREATE TABLE IF NOT EXISTS market_reviews (
    id                 TEXT PRIMARY KEY,  -- mrv_<uuid>
    listing_id         TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    buyer_workspace_id TEXT NOT NULL CHECK (buyer_workspace_id <> ''),
    rating             SMALLINT NOT NULL CHECK (rating BETWEEN 1 AND 5),
    body               TEXT NOT NULL DEFAULT '' CHECK (length(body) <= 4000),
    reply              TEXT NOT NULL DEFAULT '' CHECK (length(reply) <= 4000),
    replied_at         TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (listing_id, buyer_workspace_id),
    CHECK ((replied_at IS NULL) = (reply = ''))
);
CREATE INDEX IF NOT EXISTS idx_market_reviews_buyer ON market_reviews (buyer_workspace_id);
