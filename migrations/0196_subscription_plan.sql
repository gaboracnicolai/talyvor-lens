-- B32.10 — the plan on every subscription, and the operator's Enterprise contracts.
--
-- subscriptions.plan is set by the webhook from the lookup key of the Price the subscription bills (Team's BYOK
-- add-on, a second item, sets byok and leaves plan team). NULL is a Price that is no plan's. A BYOK row is
-- named here; every other row is named at startup from the Price ids each Service sells (Service.BackfillPlans),
-- because which Price is which plan is known to Stripe and the Service, never to the database.
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS plan TEXT
    CHECK (plan IN ('plus', 'pro', 'max', 'byok', 'team', 'business', 'enterprise'));
UPDATE subscriptions SET plan = 'byok' WHERE byok AND plan IS NULL;

-- Enterprise is contracted and invoiced by the operator, never sold through Stripe: one row while a workspace is
-- on a contract, with the contract's own fees in basis points (NULL follows internal/fees). Every set and end
-- is recorded in operator_audit in the same transaction (billing.SetContract, billing.EndContract).
CREATE TABLE IF NOT EXISTS workspace_contracts (
    workspace_id     TEXT PRIMARY KEY,
    plan             TEXT NOT NULL CHECK (plan = 'enterprise'),
    platform_fee_bps BIGINT CHECK (platform_fee_bps BETWEEN 0 AND 10000),
    fx_margin_bps    BIGINT CHECK (fx_margin_bps BETWEEN 0 AND 10000),
    reference        TEXT NOT NULL DEFAULT '',
    set_by           TEXT NOT NULL,
    set_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
