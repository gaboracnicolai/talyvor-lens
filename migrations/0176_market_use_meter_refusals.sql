-- B26.3 — one marketplace use Stripe refuses no longer blocks every later bill.
--
-- MeterPending walked the unmetered uses oldest first and stopped at the first one Stripe refused, so a
-- use that could never be metered kept every later use off its buyer's bill. A refused use now records
-- Stripe's reason and how many times it was refused, waits out a backoff before it is tried again, and
-- after five refusals is parked for an operator (GET /v1/admin/marketplace/parked-uses); the pass meters
-- every other use around it.

ALTER TABLE market_uses ADD COLUMN IF NOT EXISTS meter_refusals INTEGER NOT NULL DEFAULT 0;
ALTER TABLE market_uses ADD COLUMN IF NOT EXISTS meter_refused_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE market_uses ADD COLUMN IF NOT EXISTS meter_retry_at TIMESTAMPTZ;
ALTER TABLE market_uses ADD COLUMN IF NOT EXISTS meter_parked_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_market_uses_meter_parked ON market_uses (meter_parked_at) WHERE meter_parked_at IS NOT NULL AND metered_at IS NULL;
