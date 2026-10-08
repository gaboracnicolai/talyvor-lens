-- B32.41 — a seller's tax details: who is paid, where they live, their tax numbers and the account they are paid to,
-- as the UK reporting rules and DAC7 have a platform collect them.
--
-- One row a workspace, written by PUT /v1/workspaces/{ws}/marketplace/seller-tax, or by the reminder sweep for a
-- seller who has earnings and has not yet filled them in. seller_type is '' until the seller says which they are.
-- An individual gives first, middle and last names and a date of birth; an entity gives its legal name and company
-- registration number; both give a primary address, a country of residence, at least one TIN with the jurisdiction
-- that issued it, and the financial account they are paid to and its holder. A VAT number is checked with
-- TaxPartner.ValidateTaxID when it is saved: vat_valid is the answer, vat_detail why it is not valid.
--
-- The TINs, the date of birth and the account identifier are sealed with internal/envelope under
-- LENS_PROVIDER_SECRET_KEK: each *_sealed column is an envelope.Sealed as JSON — a key id, a wrapped data key,
-- nonces and ciphertext, and no plaintext. What is shown back is tins_masked (each TIN's jurisdiction and last
-- four characters) and account_identifier_last4.
--
-- completed_at is when the details were last found complete. reminders_sent counts the requests sent while they
-- were not: the first at the seller's first earning, then two more, LENS_SELLER_TAX_REMINDER_DAYS apart, the last
-- at last_reminded_at. withheld_since is set by the second of those reminders: the payout run skips the seller from
-- then until the details are complete, while their earnings keep clearing.

CREATE TABLE IF NOT EXISTS seller_tax_profiles (
    workspace_id                TEXT PRIMARY KEY,
    seller_type                 TEXT NOT NULL DEFAULT '' CHECK (seller_type IN ('', 'individual', 'entity')),
    first_name                  TEXT NOT NULL DEFAULT '',
    middle_name                 TEXT NOT NULL DEFAULT '',
    last_name                   TEXT NOT NULL DEFAULT '',
    legal_name                  TEXT NOT NULL DEFAULT '',
    address                     TEXT NOT NULL DEFAULT '',
    country                     TEXT NOT NULL DEFAULT '' CHECK (country = '' OR country ~ '^[A-Z]{2}$'),
    tins_sealed                 JSONB,
    tins_masked                 JSONB NOT NULL DEFAULT '[]',
    date_of_birth_sealed        JSONB,
    company_registration_number TEXT NOT NULL DEFAULT '',
    vat_number                  TEXT NOT NULL DEFAULT '',
    vat_valid                   BOOLEAN NOT NULL DEFAULT false,
    vat_detail                  TEXT NOT NULL DEFAULT '',
    vat_partner                 TEXT NOT NULL DEFAULT '',
    vat_checked_at              TIMESTAMPTZ,
    account_identifier_sealed   JSONB,
    account_identifier_last4    TEXT NOT NULL DEFAULT '',
    account_holder              TEXT NOT NULL DEFAULT '',
    self_billing_agreed_version TEXT NOT NULL DEFAULT '',
    completed_at                TIMESTAMPTZ,
    reminders_sent              INTEGER NOT NULL DEFAULT 0 CHECK (reminders_sent >= 0),
    last_reminded_at            TIMESTAMPTZ,
    withheld_since              TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (vat_valid = false OR (vat_number <> '' AND vat_checked_at IS NOT NULL)),
    CHECK (withheld_since IS NULL OR completed_at IS NULL)
);

CREATE INDEX IF NOT EXISTS idx_seller_tax_profiles_withheld
    ON seller_tax_profiles (workspace_id) WHERE withheld_since IS NOT NULL;
