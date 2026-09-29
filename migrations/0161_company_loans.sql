-- B22.5 — loans between companies: offer, borrow, repay.
--
-- An agent of one company offers an agent of another a loan in credits: the principal, the interest on it over
-- the whole term (interest_bps), how many equal instalments repay it and how often, and the fee a late
-- instalment adds. The borrower's side accepts or declines; accepting pays the principal out as a transfer
-- (B22.3). Each instalment is then taken from the borrower's agent on schedule and paid to the lender's.
-- An instalment that cannot be taken is missed: the loan is late and the instalment is tried again one period
-- later with the late fee added; missed again, the loan is in default and nothing more is taken. Both sides
-- see the state, and both statements carry the loan's terms and every instalment.
--
-- Both sides must be companies (workspaces.company, B22.4) with verified owners; credit involving a private
-- user is class RED. A loan is class AMBER (loans_between_companies): test money only until cleared.
-- Its money moves as agent_transfers rows naming the loan, which cannot be given back as a plain refund.
CREATE TABLE IF NOT EXISTS agent_loans (
    id                    TEXT PRIMARY KEY,                                   -- loan_<uuid>
    lender_workspace_id   TEXT NOT NULL,
    lender_agent_id       TEXT NOT NULL,
    borrower_workspace_id TEXT NOT NULL,
    borrower_agent_id     TEXT NOT NULL,
    principal_ulxc        BIGINT NOT NULL CHECK (principal_ulxc > 0),
    interest_bps          INT NOT NULL CHECK (interest_bps BETWEEN 0 AND 100000), -- on the principal, over the term
    instalments           INT NOT NULL CHECK (instalments BETWEEN 1 AND 120),
    every                 TEXT NOT NULL CHECK (every IN ('day', 'week', 'month')),
    late_fee_ulxc         BIGINT NOT NULL DEFAULT 0 CHECK (late_fee_ulxc >= 0),
    memo                  TEXT NOT NULL DEFAULT '',
    status                TEXT NOT NULL DEFAULT 'offered'
                          CHECK (status IN ('offered', 'declined', 'withdrawn', 'active', 'late', 'defaulted', 'repaid')),
    paid_instalments      INT NOT NULL DEFAULT 0,
    next_due_at           TIMESTAMPTZ,                                        -- when the next instalment is taken
    offered_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at            TIMESTAMPTZ,
    CHECK (lender_workspace_id <> borrower_workspace_id)
);
CREATE INDEX IF NOT EXISTS idx_agent_loans_lender ON agent_loans (lender_workspace_id, offered_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_loans_borrower ON agent_loans (borrower_workspace_id, offered_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_loans_due ON agent_loans (next_due_at) WHERE status IN ('active', 'late');

-- Every movement of a loan: the payout, each instalment taken, and each one missed.
CREATE TABLE IF NOT EXISTS agent_loan_events (
    id             BIGSERIAL PRIMARY KEY,
    loan_id        TEXT NOT NULL,                                      -- agent_loans.id
    kind           TEXT NOT NULL CHECK (kind IN ('payout', 'instalment', 'missed', 'late', 'defaulted')),
    instalment     INT NOT NULL DEFAULT 0,
    principal_ulxc BIGINT NOT NULL DEFAULT 0,
    interest_ulxc  BIGINT NOT NULL DEFAULT 0,
    late_fee_ulxc  BIGINT NOT NULL DEFAULT 0,
    transfer_id    TEXT NOT NULL DEFAULT '',
    detail         TEXT NOT NULL DEFAULT '',
    at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_loan_events_loan ON agent_loan_events (loan_id, id);
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON agent_loan_events
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON agent_loan_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

ALTER TABLE agent_transfers ADD COLUMN IF NOT EXISTS loan_id TEXT NOT NULL DEFAULT '';
