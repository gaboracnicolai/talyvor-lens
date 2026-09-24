-- 0127_subscription_allowance_fee.sql — B1.6: the fee F a period's grant was paid for.
--
-- "Your plan is $20. Your team's answers earned $6 of it back." needs F per period,
-- and F lives in Stripe (the Price), never in config or code. The webhook that grants
-- the period reads the subscription's price amount off the same payload and writes it
-- here, so the figure a subscriber sees is the amount that period was billed at, even
-- after the price changes.
--
-- 0 = unknown (a grant made before this column existed, or a non-USD price). The
-- earned-back ceiling is min(earned, fee), so an unknown fee shows nothing earned back
-- rather than an uncapped number.
--
-- Additive, own file, no row rewritten.

ALTER TABLE subscription_allowance
    ADD COLUMN IF NOT EXISTS fee_usd_cents BIGINT NOT NULL DEFAULT 0
        CHECK (fee_usd_cents >= 0);
