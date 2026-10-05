-- B32.11 — the platform fee on AI spend charged to credits, by plan (Nicolai's decision of 5 Oct 2026).
--
-- The fee is its own lxc_ledger row of type platform_fee, written in the spend's transaction; nothing here
-- stores an amount. What is stored is the rate a question was held or debited at, so its settle charges the
-- fee its hold already counted, whatever the workspace's plan is by then:
--   lxc_reservations.platform_fee_bps — the hold included the fee at this rate; the settle charges it on the
--     delivered cost and never above the hold.
--   lxc_spend_claims.platform_fee_bps — the same for a pre-serve debit (reservations off).
--   agent_postings.fee_bps            — on an agent's platform_fee posting, the rate its statement line names.
-- 0 (and NULL on agent_postings) is no fee: every row written before this migration.
ALTER TABLE lxc_reservations ADD COLUMN IF NOT EXISTS platform_fee_bps INTEGER NOT NULL DEFAULT 0
    CHECK (platform_fee_bps BETWEEN 0 AND 10000);
ALTER TABLE lxc_spend_claims ADD COLUMN IF NOT EXISTS platform_fee_bps INTEGER NOT NULL DEFAULT 0
    CHECK (platform_fee_bps BETWEEN 0 AND 10000);
ALTER TABLE agent_postings ADD COLUMN IF NOT EXISTS fee_bps INTEGER
    CHECK (fee_bps BETWEEN 0 AND 10000);
