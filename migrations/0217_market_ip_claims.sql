-- B32.47 — IP claims: notice, counter-notice, decision, and attribution as a remedy.
--
-- A workspace that believes a listing copies its work files a claim naming the original — one of its own listings, or
-- an outside reference — with its evidence and a good-faith statement. The listing stays up, but every billed use of it
-- from then on is held (a market_holds row, migration 0200, opened by the claim): its earnings stay in holdback past
-- their 14 days. The seller may counter until counter_by (LENS_IP_COUNTER_DAYS after filing); the operator then
-- decides: upheld (the listing is taken down with its refunds), attributed (a claim edge in market_lineage from the
-- listing to the original, at the share the operator set, and the holds released) or rejected (the holds released).
CREATE TABLE IF NOT EXISTS market_ip_claims (
    id                    TEXT PRIMARY KEY,  -- mic_<uuid>
    listing_id            TEXT NOT NULL REFERENCES market_listings (id) ON DELETE CASCADE,
    claimant_workspace_id TEXT NOT NULL CHECK (claimant_workspace_id <> ''),
    original_listing_id   TEXT REFERENCES market_listings (id) ON DELETE CASCADE,
    original_reference    TEXT NOT NULL DEFAULT '',
    evidence              TEXT NOT NULL CHECK (evidence <> ''),
    good_faith            TEXT NOT NULL CHECK (good_faith <> ''),
    status                TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'countered', 'upheld', 'attributed', 'rejected')),
    filed_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    counter_by            TIMESTAMPTZ NOT NULL,
    counter_statement     TEXT NOT NULL DEFAULT '',
    countered_at          TIMESTAMPTZ,
    share_bps             INTEGER NOT NULL DEFAULT 0 CHECK (share_bps BETWEEN 0 AND 10000),
    decision_reason       TEXT NOT NULL DEFAULT '',
    decided_by            TEXT NOT NULL DEFAULT '',
    decided_at            TIMESTAMPTZ,
    CHECK ((original_listing_id IS NULL) <> (original_reference = '')),  -- a listing or an outside reference, not both
    CHECK (original_listing_id IS DISTINCT FROM listing_id),
    CHECK ((decided_at IS NULL) = (status IN ('open', 'countered')) AND (decided_at IS NULL) = (decided_by = '')),
    CHECK ((status = 'attributed') = (share_bps > 0) AND (status <> 'attributed' OR original_listing_id IS NOT NULL))
);
-- One undecided claim per claimant and listing; the open ones of a listing are what holds its new uses.
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_ip_claims_undecided ON market_ip_claims (listing_id, claimant_workspace_id)
    WHERE status IN ('open', 'countered');
CREATE INDEX IF NOT EXISTS idx_market_ip_claims_claimant ON market_ip_claims (claimant_workspace_id, filed_at DESC);

-- Every billed use of a listing with an undecided claim is held by that claim, whichever path recorded it. The claim's
-- row is read FOR SHARE: a use recorded while the operator decides the claim waits for the decision, so it is either
-- held and its hold is released by the decision, or recorded after it and never held.
CREATE OR REPLACE FUNCTION market_ip_claims_hold_use() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    claim TEXT;
BEGIN
    FOR claim IN SELECT id FROM market_ip_claims WHERE listing_id = NEW.listing_id AND status IN ('open', 'countered') ORDER BY id FOR SHARE LOOP
        INSERT INTO market_holds (id, use_id, reason, opened_by, opened_at) VALUES ('mhd_' || gen_random_uuid(), NEW.id, 'ip_claim', claim, NEW.used_at);
    END LOOP;
    RETURN NEW;
END $$;
CREATE OR REPLACE TRIGGER market_ip_claims_hold_use AFTER INSERT ON market_uses
    FOR EACH ROW WHEN (NEW.charge = 'billed') EXECUTE FUNCTION market_ip_claims_hold_use();
CREATE INDEX IF NOT EXISTS idx_market_holds_opened_by ON market_holds (opened_by) WHERE released_at IS NULL;
