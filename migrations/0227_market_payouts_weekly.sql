-- B32.42 — sellers are paid weekly, each payout with a statement of its week.
--
-- The payout run pays a seller in money at most once a week, on LENS_MARKET_PAYOUT_WEEKDAY: period is the ISO week
-- (UTC) a payout was made in, such as 2026-W41, and the one-a-month unique index becomes one Stripe payout per
-- workspace and period. month stays, for history and for Stripe's $2 account fee, which is taken on a seller's first
-- payout of a calendar month only. Earnings taken as credits record their week too. Each payout's statement is read
-- from the marketplace journal for its week (internal/market/statement.go).

ALTER TABLE market_payouts ADD COLUMN IF NOT EXISTS period TEXT NOT NULL DEFAULT '';

-- The payouts made before: the ISO week each was made in — but for a second Stripe payout in one week (two months'
-- payouts a day apart), which keeps '' so the new index can be built. market_payouts is append-only but for its Stripe
-- transfer, so its guard is set aside for this one backfill.
ALTER TABLE market_payouts DISABLE TRIGGER market_payouts_transfer_only;
UPDATE market_payouts p SET period = to_char(p.created_at AT TIME ZONE 'UTC', 'IYYY-"W"IW')
 WHERE p.period = ''
   AND (p.method <> 'stripe' OR NOT EXISTS (
        SELECT 1 FROM market_payouts q
         WHERE q.workspace_id = p.workspace_id AND q.method = 'stripe' AND (q.created_at, q.id) < (p.created_at, p.id)
           AND to_char(q.created_at AT TIME ZONE 'UTC', 'IYYY-"W"IW') = to_char(p.created_at AT TIME ZONE 'UTC', 'IYYY-"W"IW')));
ALTER TABLE market_payouts ENABLE TRIGGER market_payouts_transfer_only;

DROP INDEX IF EXISTS idx_market_payouts_monthly;
CREATE UNIQUE INDEX IF NOT EXISTS idx_market_payouts_weekly ON market_payouts (workspace_id, period)
    WHERE method = 'stripe' AND period <> '';
