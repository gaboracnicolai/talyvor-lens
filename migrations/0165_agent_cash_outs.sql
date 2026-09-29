-- B22.9 — cash-out to money, behind a licensed partner.
--
-- An owner asks to turn an agent's credits back into money in their bank account. The credits leave the agent
-- at once and are held — out of the agent and out of the workspace's lxc_balances, on an account
-- cashout:<id> — while a cash-out partner pays the money out. When the partner reports it paid, the held
-- credits are gone for good (to the account 'cashed_out'); when it reports it failed, they go back to the agent.
--
-- Lens is built against one partner-neutral interface (economy.CashOutPartner). Only a test implementation
-- exists: the real one waits for a licensed partner. Cash-out is class RED (cash_out): until the operator
-- records a clearance it takes test-funded credits only, and a request of live money is refused naming the
-- class. Marketplace seller payouts through Stripe Connect (B20.5) are a different thing and are unchanged.
CREATE TABLE IF NOT EXISTS agent_cash_outs (
    id               TEXT PRIMARY KEY,                                        -- cash_<uuid>
    workspace_id     TEXT NOT NULL,
    agent_id         TEXT NOT NULL,
    amount_ulxc      BIGINT NOT NULL CHECK (amount_ulxc > 0),
    amount_uusd      BIGINT NOT NULL CHECK (amount_uusd >= 0),                -- the money it is worth, at the LXC peg
    test_funded_ulxc BIGINT NOT NULL DEFAULT 0 CHECK (test_funded_ulxc >= 0),
    destination      TEXT NOT NULL,                                           -- the owner's label for the account; the partner holds its details
    partner          TEXT NOT NULL,                                           -- which implementation handles it
    partner_ref      TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held', 'submitted', 'paid', 'failed')),
    detail           TEXT NOT NULL DEFAULT '',                                -- the partner's reason, for a failure
    requested_by     TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_agent_cash_outs_workspace ON agent_cash_outs (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_cash_outs_pending ON agent_cash_outs (created_at) WHERE status IN ('held', 'submitted');
