-- B26.15 — the test money that crossed to real agents before the wall, and its reversal.
--
-- Before B25.1 a test (synthetic) workspace's money could reach a real one: a transfer, an escrow or a market
-- use whose two sides are one test workspace and one real one. B25.1 refuses new ones and reverses none.
-- `lens synthetic-crossings` lists every such row; with --reverse it reverses each, once:
--   a transfer, or a released escrow: one agent_postings entry of kind 'reversal' (the receiving agent −amount,
--     the sending agent +amount) and an lxc_ledger row of type 'test_crossing_reversal' on each side, each
--     naming the original row;
--   a held or disputed escrow: returned to its payer by the operator (an agent_escrow_events row);
--   a billed market use: a market_refunds row of cause 'test_crossing', so it is never billed if it was not,
--     and its seller's earning, if it cleared, is reversed.
-- This table is the record of each: one row per original row, so nothing is reversed twice.
CREATE TABLE IF NOT EXISTS test_crossing_reversals (
    source            TEXT NOT NULL CHECK (source IN ('agent_transfers', 'agent_escrows', 'market_uses')),
    original_id       TEXT NOT NULL,                 -- the reversed row's id in source
    amount_ulxc       BIGINT NOT NULL CHECK (amount_ulxc > 0),
    from_workspace_id TEXT NOT NULL,                 -- the side the money is taken back from
    to_workspace_id   TEXT NOT NULL,                 -- the side it is returned to
    entry_id          TEXT NOT NULL DEFAULT '',      -- the agent_postings entry that moved it back, if any
    operator          TEXT NOT NULL,
    reversed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source, original_id)
);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON test_crossing_reversals
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON test_crossing_reversals
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- A market use's reversal is a refund of a new cause. Like a buyer's refund it credits nobody's bill.
ALTER TABLE market_refunds DROP CONSTRAINT IF EXISTS market_refunds_cause_check;
ALTER TABLE market_refunds ADD CONSTRAINT market_refunds_cause_check
    CHECK (cause IN ('takedown', 'buyer_refund', 'chargeback', 'test_crossing'));
