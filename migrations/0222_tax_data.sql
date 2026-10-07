-- B32.37 — tax as data: the jurisdictions Talyvor accounts for tax in, their rates, and its registrations.
--
-- Every rate and registration is a row here, never a literal in the code: internal/partners' Test tax partner
-- decides from these rows, and a real partner (B32.45) answers from its own. All three tables are empty until
-- the operator loads them — `lens tax rates import <csv>` and `lens tax registrations add`.
--
-- A jurisdiction is an ISO 3166-1 alpha-2 country. union_code names the union whose one-stop registration also
-- covers it — EU for a member state, so one EU OSS registration covers all of them. rate_source is where a tax
-- amount is converted to the jurisdiction's currency on a receipt (B32.40); registration_from_first_sale marks a
-- place where a supplier must register before its first consumer sale (B32.39).
--
-- A rate holds from valid_from until valid_to (open when NULL), in basis points: 100 is 1%. A registration is
-- Talyvor's own, under a scheme such as GB VAT or EU OSS non-Union, from effective_from; its jurisdiction is a
-- jurisdiction's code or a union's.

CREATE TABLE IF NOT EXISTS tax_jurisdictions (
    code                         TEXT PRIMARY KEY CHECK (code ~ '^[A-Z]{2}$'),
    currency                     TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    rate_source                  TEXT NOT NULL DEFAULT 'ecb' CHECK (rate_source <> ''),
    registration_from_first_sale BOOLEAN NOT NULL DEFAULT false,
    union_code                   TEXT CHECK (union_code ~ '^[A-Z]{2}$'),
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tax_rates (
    jurisdiction TEXT NOT NULL REFERENCES tax_jurisdictions (code),
    tax_code     TEXT NOT NULL CHECK (tax_code ~ '^[a-z][a-z0-9_]*$'),
    rate_bps     INTEGER NOT NULL CHECK (rate_bps BETWEEN 0 AND 10000),
    valid_from   TIMESTAMPTZ NOT NULL,
    valid_to     TIMESTAMPTZ CHECK (valid_to > valid_from),
    source       TEXT NOT NULL CHECK (source <> ''),
    PRIMARY KEY (jurisdiction, tax_code, valid_from)
);

CREATE TABLE IF NOT EXISTS tax_registrations (
    jurisdiction   TEXT NOT NULL CHECK (jurisdiction ~ '^[A-Z]{2}$'),
    scheme         TEXT NOT NULL CHECK (scheme <> ''),
    number         TEXT NOT NULL CHECK (number <> ''),
    effective_from TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (jurisdiction, scheme)
);
