-- 0058_u6_pr2_ratecap_linkage.sql
-- U6 PR2 (Sybil wash-hardening fast-follow). Default-safe; nothing turns the
-- economy on. The verified-to-earn FLOOR (PR1) raised the entry bar but not the
-- steady-state wash yield; this adds the universal bound + a cheap linkage.
--
-- 1) Covering index for the per-identity mint RATE CAP's rolling-window SUM
--    (mining.checkMintRateCap): SUM(amount) over a workspace's mint rows in the
--    window. INCLUDE (type, amount) makes it an INDEX-ONLY scan — the type
--    filter (the floor's mintTypeList, passed dynamically so this DDL carries NO
--    type list and cannot diverge from the gate set) and the amount sum are read
--    from the index leaf with no heap fetch, bounded by (workspace_id,
--    created_at-window). The existing idx_ledger_workspace (workspace_id,
--    created_at DESC) bounds the window but heap-fetches every row to filter
--    type / sum amount; this covering index removes that on the mint hot path.
CREATE INDEX IF NOT EXISTS idx_ledger_mint_rate
    ON lens_token_ledger (workspace_id, created_at) INCLUDE (type, amount);

-- 2) Owner-linkage signal for the pool-royalty wash check (the cheap bonus): the
--    SET of card fingerprints a workspace has paid with. Captured BEST-EFFORT
--    from the Stripe webhook AFTER the credit commits (a capture failure never
--    affects the payment). We store a HASH of the fingerprint, never the raw
--    value. ON CONFLICT DO NOTHING accumulates the set so a later second card
--    cannot erase the linking first card — the linkage join matches on ANY
--    shared hash. The fingerprint_hash index serves that join. Catches the lazy
--    one-card-many-workspaces operator; an operator rotating cards evades it
--    (the rate cap still bounds yield regardless).
CREATE TABLE IF NOT EXISTS workspace_card_fingerprints (
    workspace_id     TEXT NOT NULL,
    fingerprint_hash TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, fingerprint_hash)
);
CREATE INDEX IF NOT EXISTS idx_card_fingerprint ON workspace_card_fingerprints (fingerprint_hash);
