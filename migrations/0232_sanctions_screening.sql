-- B30.6 — sanctions screening of every payee and every outside payment (internal/screening).
--
-- screening_lists is each sanctions list Lens screens against — the UK Sanctions List and OFAC's SDN list — with when
-- it was last loaded and, when the last download failed, why: a failed download leaves the copy already loaded in
-- force. screening_entries is every name and alias on the copy in force, as published and normalised; a list's rows
-- are replaced in one transaction, so a screening never sees half a list.
--
-- compliance_cases is each match a screening found: an exact one is blocked, a close one is held until an operator
-- releases or refuses it. One case per subject, name, direction and currency, so a retried payment reads its case
-- rather than opening another; a release counts only for the amount, funding and capability it was decided on.
-- B30.7 and B30.8 add their own kinds of case to the same table.

CREATE TABLE IF NOT EXISTS screening_lists (
    list         TEXT PRIMARY KEY CHECK (list <> ''),
    source_url   TEXT NOT NULL DEFAULT '',
    version      BIGINT NOT NULL DEFAULT 0,   -- 0 until the list is first loaded; one more each load
    entries      INTEGER NOT NULL DEFAULT 0,
    published    TEXT NOT NULL DEFAULT '',    -- the date the list itself prints
    loaded_at    TIMESTAMPTZ,
    attempted_at TIMESTAMPTZ,
    last_error   TEXT NOT NULL DEFAULT '',    -- why the last download failed; '' once one succeeds
    failed_at    TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS screening_entries (
    list     TEXT NOT NULL REFERENCES screening_lists (list),
    entry_id TEXT NOT NULL,                  -- the list's own id for the person, entity or ship
    name     TEXT NOT NULL CHECK (name <> ''),
    norm     TEXT NOT NULL CHECK (norm <> ''),
    alias    BOOLEAN NOT NULL DEFAULT false,
    weak     BOOLEAN NOT NULL DEFAULT false, -- a low-quality alias: a match on it is held, never blocked
    kind     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_screening_entries_list ON screening_entries (list);

CREATE TABLE IF NOT EXISTS compliance_cases (
    id            TEXT PRIMARY KEY CHECK (id <> ''),
    workspace_id  TEXT NOT NULL CHECK (workspace_id <> ''),
    kind          TEXT NOT NULL CHECK (kind IN ('screening')),
    subject_kind  TEXT NOT NULL CHECK (subject_kind IN ('payee', 'payment')),
    subject_id    TEXT NOT NULL CHECK (subject_id <> ''),
    name          TEXT NOT NULL CHECK (name <> ''),
    outcome       TEXT NOT NULL CHECK (outcome IN ('hit', 'review')),
    status        TEXT NOT NULL CHECK (status IN ('blocked', 'held', 'released', 'refused')),
    matches       JSONB NOT NULL DEFAULT '[]',
    provider      TEXT NOT NULL DEFAULT '',
    capability    TEXT NOT NULL DEFAULT '',
    direction     TEXT NOT NULL DEFAULT '' CHECK (direction IN ('', 'in', 'out')),
    amount_minor  BIGINT,
    currency      TEXT NOT NULL DEFAULT '',
    funding       TEXT NOT NULL DEFAULT '',
    opened_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by    TEXT NOT NULL DEFAULT '',
    decided_at    TIMESTAMPTZ,
    decision_note TEXT NOT NULL DEFAULT '',
    UNIQUE (workspace_id, subject_kind, subject_id, name, direction, currency)
);
CREATE INDEX IF NOT EXISTS idx_compliance_cases_status ON compliance_cases (status, opened_at DESC);

-- Who an outside payment is from or to: the payer of money in, the payee of money out. '' for money that does not
-- leave or enter through a partner.
ALTER TABLE money_entries ADD COLUMN IF NOT EXISTS counterparty TEXT NOT NULL DEFAULT '';
