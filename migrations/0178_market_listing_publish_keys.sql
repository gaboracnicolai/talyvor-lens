-- B17.34 — a publish sent again with the same Idempotency-Key publishes once.
--
-- A publish that meets a deploy's restart is answered 502 by the proxy, and whoever sent it cannot tell
-- whether the listing was made. Sent again with the same Idempotency-Key, it answers the listing that key
-- already published in that workspace and makes no second one. A publish without a key is unchanged (NULL).
ALTER TABLE market_listings ADD COLUMN IF NOT EXISTS publish_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_listings_publish_key
    ON market_listings (workspace_id, publish_key) WHERE publish_key IS NOT NULL;
