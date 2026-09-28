-- B22.1 — every wallet capability carries its class, and real money obeys it.
--
-- GREEN capabilities take real money now. AMBER ones take test money, and real money only once a lawyer has
-- confirmed them; RED ones take test money only, until a licence or a licensed partner exists. An AMBER or RED
-- capability takes only TEST-FUNDED credits until the operator records a clearance for it (wallet_clearances,
-- below); a GREEN one takes any.
--
-- Every credit lot carries its funding on its lxc_ledger row (metadata.funding: test — bought in Stripe test
-- mode —, live, grant or synthetic). What part of a workspace's balance is test-funded is one scalar,
-- lxc_balances.test_funded_ulxc — lot accounting collapsed like cash_backed_ulxc (0115):
--
--   · a test-mode purchase adds to it;
--   · an AMBER or RED spend without a clearance takes from it, and is refused when it does not cover it;
--   · every other spend takes the credits that are NOT test-funded first: the balance writer clamps it to
--     the balance plus the workspace's open reservation holds, so a hold (which is not a spend) takes nothing.
ALTER TABLE lxc_balances
  ADD COLUMN IF NOT EXISTS test_funded_ulxc BIGINT NOT NULL DEFAULT 0 CHECK (test_funded_ulxc >= 0);

-- Backfill: what test-mode purchases credited, up to the balance. Exact while no live purchase exists (Stripe
-- has run on test keys only); where a grant was spent before a test purchase it counts that grant as test.
UPDATE lxc_balances b
   SET test_funded_ulxc = LEAST(GREATEST(b.balance, 0), p.test_ulxc)
  FROM (SELECT workspace_id, sum(lxc_amount) AS test_ulxc FROM lxc_purchases
         WHERE NOT livemode AND status = 'completed' AND lxc_amount > 0 GROUP BY workspace_id) p
 WHERE p.workspace_id = b.workspace_id AND b.test_funded_ulxc = 0;

-- The balance writer sums a workspace's open holds on every write that has test-funded credits to clamp.
CREATE INDEX IF NOT EXISTS idx_lxc_reservations_held_workspace
    ON lxc_reservations (workspace_id)
    WHERE status = 'held';

-- The operator's clearances: each clear and each revoke is a row, append-only, so the table IS the audit log.
-- A capability is cleared while its latest row is a 'clear'.
CREATE TABLE IF NOT EXISTS wallet_clearances (
    id         BIGSERIAL PRIMARY KEY,
    capability TEXT NOT NULL,
    action     TEXT NOT NULL CHECK (action IN ('clear', 'revoke')),
    operator   TEXT NOT NULL CHECK (operator <> ''),               -- who
    reference  TEXT NOT NULL CHECK (reference <> ''),              -- the lawyer's or partner's reference; why, on a revoke
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()                  -- when
);
CREATE INDEX IF NOT EXISTS idx_wallet_clearances_capability ON wallet_clearances (capability, id DESC);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON wallet_clearances
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON wallet_clearances
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- Cards are RED: what test-funded credits each approved purchase took, and what each settlement gave back
-- (+) or took (−), so a reversed test purchase returns test-funded credits.
ALTER TABLE agent_card_authorizations ADD COLUMN IF NOT EXISTS test_funded_ulxc BIGINT NOT NULL DEFAULT 0;
ALTER TABLE agent_card_settlements ADD COLUMN IF NOT EXISTS test_funded_ulxc BIGINT NOT NULL DEFAULT 0;

-- Test money never reaches a live payout: each earning is live or test as the invoice that paid it was, and
-- each Stripe payout as the key that made it was. A live key pays out only live earnings. Everything before
-- this migration was test (Stripe has run on test keys only).
ALTER TABLE market_earnings ADD COLUMN IF NOT EXISTS livemode BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE market_payouts ADD COLUMN IF NOT EXISTS livemode BOOLEAN NOT NULL DEFAULT false;
