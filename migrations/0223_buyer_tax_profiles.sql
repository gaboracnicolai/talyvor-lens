-- B32.38 — a buyer's tax profile: who the workspace says it is and where, for the tax on its marketplace sales.
--
-- One row a workspace, written by PUT /v1/workspaces/{ws}/tax-profile. country is the declared country (ISO 3166-1
-- alpha-2). A tax id is checked with TaxPartner.ValidateTaxID when it is saved: tax_id is the number as the partner
-- normalised it, tax_id_valid its answer and tax_id_checked_at when it was asked; an invalid number is kept, marked
-- invalid, and the workspace stays a consumer. declared_at is the last time the workspace saved the profile.
--
-- taxprofile.Store.Resolve weighs the declared country against the billing country of the workspace's Stripe
-- customer and the card country of its default payment method. When they conflict the declared country is used
-- and the profile is flagged for the operator: flagged_at and flag_reason, cleared when the workspace declares
-- another country.

CREATE TABLE IF NOT EXISTS buyer_tax_profiles (
    workspace_id      TEXT PRIMARY KEY,
    legal_name        TEXT NOT NULL DEFAULT '',
    address           TEXT NOT NULL DEFAULT '',
    country           TEXT NOT NULL CHECK (country ~ '^[A-Z]{2}$'),
    region            TEXT NOT NULL DEFAULT '',
    postal_code       TEXT NOT NULL DEFAULT '',
    business          BOOLEAN NOT NULL DEFAULT false,
    tax_id            TEXT,
    tax_id_checked_at TIMESTAMPTZ,
    tax_id_valid      BOOLEAN NOT NULL DEFAULT false,
    tax_id_detail     TEXT NOT NULL DEFAULT '',
    tax_id_partner    TEXT NOT NULL DEFAULT '',
    declared_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    flagged_at        TIMESTAMPTZ,
    flag_reason       TEXT NOT NULL DEFAULT '',
    CHECK (tax_id_valid = false OR (tax_id IS NOT NULL AND tax_id_checked_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_buyer_tax_profiles_flagged
    ON buyer_tax_profiles (flagged_at) WHERE flagged_at IS NOT NULL;
