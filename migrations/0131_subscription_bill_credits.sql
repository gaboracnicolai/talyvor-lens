-- 0131_subscription_bill_credits.sql — B13.2: a subscriber's earnings, credited against their next bill.
--
-- One row per renewal invoice that received a credit. invoice_id is the primary key, so a redelivered
-- invoice.created can never credit twice. credited_ulens is the LENS the credit consumed (debited from
-- the workspace's balance in the same transaction, lens_token_ledger type subscription_bill_credit);
-- credited_usd_cents is the negative line added to the invoice. A bill credit, never a payout.

CREATE TABLE IF NOT EXISTS subscription_bill_credits (
    invoice_id             TEXT        PRIMARY KEY,
    workspace_id           TEXT        NOT NULL,
    stripe_subscription_id TEXT        NOT NULL,
    credited_ulens         BIGINT      NOT NULL CHECK (credited_ulens > 0),
    credited_usd_cents     BIGINT      NOT NULL CHECK (credited_usd_cents > 0),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS subscription_bill_credits_ws_idx ON subscription_bill_credits (workspace_id);
