-- B22.3 — send and request money between any agents on Talyvor.
--
-- Every wallet has an address: its wallet ID (the agent's id) and, once its owner picks one, a handle. Any
-- agent — of a company or of a private user — can send credits to any other agent, ask one for credits, accept
-- or decline what it is asked, give a transfer back, and pay another agent on a schedule. Both owners must be
-- verified (B19.11), and the sender's rules (B19.2) judge every transfer as spending. Between agents of one
-- owner a transfer is GREEN; between different owners it is AMBER (B22.1): test money only until cleared.
--
-- Credits stay credits: a transfer moves LXC from the sender's workspace to the receiver's, where it can be
-- spent only in the Talyvor network. It is ONE agent_postings entry of TWO postings that sum to zero —
-- agent:<sender> −amount in the sender's workspace, agent:<receiver> +amount in the receiver's — and, between
-- workspaces, an lxc_ledger row of type 'agent_transfer' on each side. Test-funded credits arrive test-funded.
ALTER TABLE agent_accounts ADD COLUMN IF NOT EXISTS handle TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_accounts_handle ON agent_accounts (lower(handle)) WHERE handle IS NOT NULL;

CREATE TABLE IF NOT EXISTS agent_transfers (
    id                TEXT PRIMARY KEY,                            -- xfer_<uuid>
    entry_id          UUID NOT NULL,                               -- its agent_postings entry
    from_workspace_id TEXT NOT NULL,
    from_agent_id     TEXT NOT NULL,
    to_workspace_id   TEXT NOT NULL,
    to_agent_id       TEXT NOT NULL,
    amount_ulxc       BIGINT NOT NULL CHECK (amount_ulxc > 0),
    memo              TEXT NOT NULL DEFAULT '',
    class             TEXT NOT NULL CHECK (class IN ('GREEN', 'AMBER')),
    test_funded_ulxc  BIGINT NOT NULL DEFAULT 0 CHECK (test_funded_ulxc >= 0), -- of it, test money
    request_id        TEXT NOT NULL DEFAULT '',                    -- the request it answered
    schedule_id       TEXT NOT NULL DEFAULT '',                    -- the schedule that made it
    refund_of         TEXT NOT NULL DEFAULT '',                    -- the transfer it gave back
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_agent_id <> to_agent_id)
);
CREATE INDEX IF NOT EXISTS idx_agent_transfers_from ON agent_transfers (from_agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_transfers_to ON agent_transfers (to_agent_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_transfers_refund ON agent_transfers (refund_of) WHERE refund_of <> '';

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON agent_transfers
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON agent_transfers
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- One agent asks another for credits; the asked agent's side accepts (which makes the transfer) or declines.
CREATE TABLE IF NOT EXISTS agent_money_requests (
    id                TEXT PRIMARY KEY,                            -- mreq_<uuid>
    from_workspace_id TEXT NOT NULL,                               -- the asking agent's: it is paid
    from_agent_id     TEXT NOT NULL,
    to_workspace_id   TEXT NOT NULL,                               -- the asked agent's: it pays
    to_agent_id       TEXT NOT NULL,
    amount_ulxc       BIGINT NOT NULL CHECK (amount_ulxc > 0),
    memo              TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'declined')),
    transfer_id       TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at        TIMESTAMPTZ,
    CHECK (from_agent_id <> to_agent_id)
);
CREATE INDEX IF NOT EXISTS idx_agent_money_requests_to ON agent_money_requests (to_workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_money_requests_from ON agent_money_requests (from_workspace_id, created_at DESC);
