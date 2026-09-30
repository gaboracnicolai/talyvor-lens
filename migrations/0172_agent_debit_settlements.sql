-- 0172_agent_debit_settlements.sql — B23.13: with reservations off, an agent pays what its question actually cost.
--
-- With LENS_LXC_RESERVATION_ENABLED=false an agent's question is charged by the pre-serve SpendLXCForAgent debit,
-- which is the input-only estimate. After the serve the difference between the delivered cost and that estimate
-- is settled in one lxc_ledger row: charged (up to what the agent's limit still allows) or refunded. One row
-- here per settled question — its exactly-once claim, keyed on the same debit key as lxc_spend_claims — and the
-- part of the delivered cost the limit did not allow, written off, with the question it belongs to.

CREATE TABLE IF NOT EXISTS agent_debit_settlements (
    debit_key        TEXT        PRIMARY KEY,         -- lxc_spend_claims.request_id of the pre-serve debit
    workspace_id     TEXT        NOT NULL,
    scoped_key_id    TEXT        NOT NULL,
    agent_id         TEXT,                            -- the agent account the key spends for; NULL for a key with none
    request_id       TEXT,                            -- the question: token_events request_id
    estimate_ulxc    BIGINT      NOT NULL,            -- the pre-serve debit
    delivered_ulxc   BIGINT      NOT NULL,            -- what the answer actually cost
    settled_ulxc     BIGINT      NOT NULL,            -- the settling row: > 0 charged, < 0 refunded
    written_off_ulxc BIGINT      NOT NULL DEFAULT 0 CHECK (written_off_ulxc >= 0),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (estimate_ulxc + settled_ulxc + written_off_ulxc = delivered_ulxc)
);

CREATE INDEX IF NOT EXISTS agent_debit_settlements_written_off
    ON agent_debit_settlements (workspace_id, created_at) WHERE written_off_ulxc > 0;
