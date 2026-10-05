-- B32.8 — Talyvor keeps 15% of every listing sale from the first dollar (LENS_MARKET_TAKE_BPS, internal/fees), and
-- 5% of a payment to another company's agent (LENS_SERVICES_TAKE_BPS). Each earning records Talyvor's fee
-- beside the seller's share: gross = share + fee.
--
-- An earning cleared before this migration was split by the US$1M tier and keeps those terms (the table is
-- append-only, 0149): its fee reads 0.
ALTER TABLE market_earnings ADD COLUMN IF NOT EXISTS fee_usd_micros BIGINT NOT NULL DEFAULT 0;
