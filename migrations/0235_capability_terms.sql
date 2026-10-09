-- B30.9 — terms for each capability, accepted before first use (economy/capability_terms.go).
--
-- capability_terms is every version of a wallet capability's terms: where its text is (docs/terms/<capability>.md) and
-- the words themselves, so an acceptance always names the exact text it accepted. The latest version is the one in
-- force. A capability with terms refuses use, test money or live, by a workspace that has not accepted its latest
-- version; publishing a new version (`lens terms publish`) asks every workspace again.
--
-- capability_terms_acceptances is each acceptance: the workspace, the person, the version, when, and the address it
-- came from as an HMAC of the IP under capability_terms_ip_key — never the address itself.
--
-- Both are append-only: a version is never rewritten, and an acceptance is evidence.

CREATE TABLE IF NOT EXISTS capability_terms (
    capability   TEXT NOT NULL CHECK (capability <> ''),
    version      INTEGER NOT NULL CHECK (version > 0),
    text_path    TEXT NOT NULL CHECK (text_path <> ''),
    body         TEXT NOT NULL CHECK (body <> ''),
    body_sha256  TEXT NOT NULL CHECK (body_sha256 ~ '^[0-9a-f]{64}$'),
    published_by TEXT NOT NULL CHECK (published_by <> ''),
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (capability, version)
);

CREATE TABLE IF NOT EXISTS capability_terms_acceptances (
    id           BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL CHECK (workspace_id <> ''),
    capability   TEXT NOT NULL,
    version      INTEGER NOT NULL,
    person       TEXT NOT NULL CHECK (person <> ''),
    ip_hash      TEXT NOT NULL,
    accepted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (capability, version) REFERENCES capability_terms (capability, version),
    UNIQUE (workspace_id, capability, version)
);

-- The key the acceptances' IP addresses are hashed under: one row, made by Lens on first use.
CREATE TABLE IF NOT EXISTS capability_terms_ip_key (
    id         BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    key        BYTEA NOT NULL CHECK (length(key) = 32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON capability_terms
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON capability_terms
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON capability_terms_acceptances
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON capability_terms_acceptances
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
