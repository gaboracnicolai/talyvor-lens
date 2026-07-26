-- 0109_lxc_purchases_livemode.sql — record whether a purchase was REAL money.
--
-- lxc_purchases is the verified-to-earn evidence (internal/earnverify): a completed
-- row with lxc_amount > 0 makes a workspace eligible to MINT. Until now the row
-- said nothing about which Stripe mode produced it, and no code branched on mode —
-- so a TEST-mode purchase (card 4242…, no money) wrote a row byte-for-byte
-- identical to a live one and conferred the same permanent earning rights. With
-- test keys enabled for a trial, anyone holding a workspace API key could mint
-- themselves earning rights for free.
--
-- livemode comes from what Stripe reports on the EVENT (Event.Livemode), not from
-- inspecting the API key: the event is the thing that is signed, so it cannot be
-- forged by a caller and cannot drift from the key actually in use.
--
-- NULLABLE ON PURPOSE. NULL means "recorded before this column existed", which is
-- an honest third state rather than a guess. No backfill is performed: billing has
-- never been enabled on any deployment (the checkout and webhook routes are
-- registered only under LENS_BILLING_ENABLED, default false, and the webhook
-- handler is the ONLY writer of this table), so there are no historical rows to
-- classify — but if some deployment does hold rows, defaulting them to `true`
-- would silently assert they were real money, which is the one thing this column
-- exists to stop.
--
-- The earn predicate requires livemode IS NOT NULL, so a legacy NULL row stops
-- conferring earning rights until someone looks at it deliberately.

ALTER TABLE lxc_purchases ADD COLUMN IF NOT EXISTS livemode BOOLEAN;

COMMENT ON COLUMN lxc_purchases.livemode IS
  'Stripe Event.Livemode for the crediting event: true = real money, false = test mode, NULL = recorded before 0109. Consumed by internal/earnverify.';
