-- 0130_plan_included_usage.sql — B13.1: each plan's included usage D, per month.
--
-- D is computed from the plan's price F and the measured pooled share h (internal/billing/
-- included_usage.go), once a month, at the month's first grant for that price. Stored so every grant
-- that month uses the same figure, and so next month's rise can be capped at 25% of this one.
--
-- Additive, own table.

CREATE TABLE IF NOT EXISTS plan_included_usage (
    month           DATE             NOT NULL,           -- the 1st of the month the figure applies to
    fee_usd_cents   BIGINT           NOT NULL CHECK (fee_usd_cents > 0),
    pooled_share    DOUBLE PRECISION NOT NULL CHECK (pooled_share >= 0 AND pooled_share < 1), -- h used
    window_requests BIGINT           NOT NULL CHECK (window_requests >= 0), -- subscriber chat requests measured
    included_ulxc   BIGINT           NOT NULL CHECK (included_ulxc >= 0),   -- D
    computed_at     TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (month, fee_usd_cents)
);
