-- 0171_refund_paid_currency.sql — B23.6: a refund of a top-up paid in another currency is recorded in US dollars.
--
-- Since B22.2 a top-up can be charged in the customer's own currency, and charge.refunded reports its amounts
-- in THAT currency. refunded_cents and amount_refunded_cents stay US cents — converted at the charge's own rate,
-- the purchase's usd_cents over what was paid for them — and the amount refunded in the paid currency sits beside
-- them, so a €2.00 refund of a €9.18 / $10.00 top-up reads 218 US cents and 200 EUR cents, not 200 "US" cents.
-- '' is the honest currency for every row written before this: the event did not say, or was not read.

ALTER TABLE lxc_purchases
    ADD COLUMN IF NOT EXISTS paid_currency        TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS refunded_paid_amount BIGINT NOT NULL DEFAULT 0;

ALTER TABLE billing_refunds
    ADD COLUMN IF NOT EXISTS paid_currency        TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS amount_refunded_paid BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN lxc_purchases.paid_currency IS
  'The currency the customer was charged in (the checkout session''s, lower-case); '''' on rows predating B23.6.';
COMMENT ON COLUMN lxc_purchases.refunded_paid_amount IS
  'Cumulative amount refunded, in paid_currency minor units — the figure refunded_cents is the US-cent value of.';
COMMENT ON COLUMN billing_refunds.amount_refunded_paid IS
  'Stripe charge.amount_refunded in paid_currency. amount_refunded_cents is its US-cent value, 0 until the purchase is seen when the charge was not in USD.';
