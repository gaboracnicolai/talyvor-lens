-- 0079_agent_lxc_subbudgets.sql — F4-capstone step A: per-scoped-key LXC sub-budget + the exactly-once
-- spend-claim ledger. Substrate for the closed-loop agent-allocation capstone (the allocator itself is a
-- later step). MONEY-adjacent but MINTS NOTHING — this bounds/records SPENDING of already-existing LXC, it
-- credits no ledger and adds no mint type. CREATE-only, additive: no ALTER/DROP on lxc_balances / lxc_ledger
-- / any existing economy table.
--
-- CLOSED-LOOP + CENTRAL-COUNTERPARTY: LXC is one-way (fiat/LENS → LXC → spent-to-pool; never LXC→fiat, never
-- account↔account). The agent's actual LXC is debited from its WORKSPACE's lxc_balances (workspace↔Talyvor
-- pool) via the existing SpendLXC internals; this sub-budget is only a per-sub-identity CEILING on top.

-- The depleting per-scoped-key sub-budget: an absolute cap (ceiling_lxc) on total LXC a scoped API key (the
-- "agent") may spend, with a monotonic spent_lxc. remaining = ceiling_lxc - spent_lxc. Default ceiling 50 LXC
-- ($5 at the 1 LXC = $0.10 peg) applied when a key has no explicit ceiling. Bound to the scoped key, NOT the
-- workspace — a sub-identity ceiling UNDER the workspace's LXC balance.
CREATE TABLE IF NOT EXISTS agent_lxc_subbudgets (
    scoped_key_id TEXT             PRIMARY KEY,             -- the scoped API key = the "agent" identity
    workspace_id  TEXT             NOT NULL,                -- the parent workspace whose lxc_balances funds the spend
    ceiling_lxc   DOUBLE PRECISION NOT NULL DEFAULT 50,     -- absolute LXC the agent may spend total
    spent_lxc     DOUBLE PRECISION NOT NULL DEFAULT 0,      -- monotonic; never exceeds ceiling_lxc (enforced in tx)
    updated_at    TIMESTAMPTZ      NOT NULL DEFAULT now()
);

-- The idempotency ledger: request_id PRIMARY KEY = exactly-once. A successful agent debit INSERTs its
-- request_id here IN THE SAME TRANSACTION as the balance decrement + spent_lxc bump — so a retry/concurrent
-- call with the same request_id debits NOTHING (ON CONFLICT DO NOTHING ⇒ 0 rows ⇒ idempotent replay). A
-- REJECTED spend rolls this row back too (no orphan claim), so the request_id stays retriable after funding.
CREATE TABLE IF NOT EXISTS lxc_spend_claims (
    request_id    TEXT             PRIMARY KEY,             -- the exactly-once key (one debit per request)
    scoped_key_id TEXT             NOT NULL,                -- the agent that spent
    lxc_amount    DOUBLE PRECISION NOT NULL,                -- the LXC debited under this request
    created_at    TIMESTAMPTZ      NOT NULL DEFAULT now()
);
