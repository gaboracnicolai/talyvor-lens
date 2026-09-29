-- B22.6 — escrow: money held until the deal is done.
--
-- An agent pays into escrow for another agent: the credits leave the payer and are held — by Talyvor, in no
-- workspace's balance — until they are released to the payee. The payer's side confirms delivery, or the
-- agreed deadline (release_at) passes without a dispute, and they are released. A dispute, raised by the
-- payer's side before the deadline, holds them until the operator decides: released to the payee, or
-- returned to the payer.
--
-- Its money is agent_postings entries of kind 'escrow' on an account 'escrow:<id>' in the payer's workspace:
-- paying in is agent:<payer> −amount / escrow:<id> +amount, and the LXC leaves the payer's lxc_balances (an
-- lxc_ledger row of type 'agent_escrow'); releasing is escrow:<id> −amount / agent:<payee> +amount, and the LXC
-- reaches the payee's workspace; returning is escrow:<id> −amount / agent:<payer> +amount. Test-funded credits
-- come out test-funded wherever they go.
--
-- The payer's rules (B19.2) judge paying in as spending, and both owners must be verified (B19.11). Between one
-- owner's agents it is GREEN; between different owners it is class AMBER (escrow, B22.1): test money only
-- until cleared.
CREATE TABLE IF NOT EXISTS agent_escrows (
    id                 TEXT PRIMARY KEY,                                     -- escrow_<uuid>
    payer_workspace_id TEXT NOT NULL,
    payer_agent_id     TEXT NOT NULL,
    payee_workspace_id TEXT NOT NULL,
    payee_agent_id     TEXT NOT NULL,
    amount_ulxc        BIGINT NOT NULL CHECK (amount_ulxc > 0),
    memo               TEXT NOT NULL DEFAULT '',
    class              TEXT NOT NULL CHECK (class IN ('GREEN', 'AMBER')),
    test_funded_ulxc   BIGINT NOT NULL DEFAULT 0 CHECK (test_funded_ulxc >= 0), -- of it, test money
    release_at         TIMESTAMPTZ NOT NULL,                                 -- the deadline: released then, undisputed
    status             TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held', 'disputed', 'released', 'returned')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at         TIMESTAMPTZ,
    CHECK (payer_agent_id <> payee_agent_id)
);
CREATE INDEX IF NOT EXISTS idx_agent_escrows_payer ON agent_escrows (payer_workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_escrows_payee ON agent_escrows (payee_workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_escrows_due ON agent_escrows (release_at) WHERE status = 'held';
CREATE INDEX IF NOT EXISTS idx_agent_escrows_disputed ON agent_escrows (created_at) WHERE status = 'disputed';

-- Every state an escrow passes through: held, disputed, released, returned — who moved it and why.
CREATE TABLE IF NOT EXISTS agent_escrow_events (
    id        BIGSERIAL PRIMARY KEY,
    escrow_id TEXT NOT NULL,                                                 -- agent_escrows.id
    kind      TEXT NOT NULL CHECK (kind IN ('held', 'disputed', 'released', 'returned')),
    actor     TEXT NOT NULL CHECK (actor IN ('payer', 'deadline', 'operator')),
    operator  TEXT NOT NULL DEFAULT '',                                      -- who decided, for 'operator'
    detail    TEXT NOT NULL DEFAULT '',                                      -- the dispute's reason, the decision's note
    entry_id  TEXT NOT NULL DEFAULT '',                                      -- the agent_postings entry that moved it
    at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_escrow_events_escrow ON agent_escrow_events (escrow_id, id);
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON agent_escrow_events
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON agent_escrow_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
