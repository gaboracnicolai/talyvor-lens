-- B30.11 — daily reconciliation against the partner, and the safeguarding view.
--
-- A partner money account (0221) mirrors one account at the account partner: partner_account_ref names it. An entry
-- that moved money in or out through a partner names the partner's reference for that payment, partner_ref, so its
-- postings can be matched to the partner's statement lines. Both default to '' for what was written before.
--
-- Each day, for each currency, the reconciliation compares the partner accounts' postings with the partner's
-- statement lines, and writes one reconciliation_runs row with every break it found: a posting the statement does
-- not have (missing), a statement line the ledger does not have (extra), or one whose amounts differ. The same row
-- keeps what customers hold in that currency against what the partner reports holding for them — the safeguarding
-- view reads the latest.

ALTER TABLE money_accounts ADD COLUMN IF NOT EXISTS partner_account_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE money_entries ADD COLUMN IF NOT EXISTS partner_ref TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS reconciliation_runs (
    id                   TEXT PRIMARY KEY,                          -- rec_<uuid>
    day                  DATE NOT NULL,                             -- the UTC day whose postings and lines it compared
    currency             TEXT NOT NULL,
    funding              TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    partner              TEXT NOT NULL,                             -- the partner's Name(): "test" for the Test partner
    customers_hold_minor BIGINT NOT NULL,                           -- company, agent, pot, hold and suspense accounts, when it ran
    partner_holds_minor  BIGINT NOT NULL,                           -- the partner's statements' total, when it ran
    break_count          INT NOT NULL CHECK (break_count >= 0),
    breaks               JSONB NOT NULL DEFAULT '[]',
    ran_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_reconciliation_runs_latest ON reconciliation_runs (currency, funding, ran_at DESC);

-- A run is a record of what was found: it is never changed.
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON reconciliation_runs
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
