-- B18.13 — documents a workspace uploads (POST /v1/documents, up to 25 MB) so a chat request references
-- one by id instead of carrying the file in its body, which the 4 MiB request cap limited to about 2.5 MB.
-- The proxy reads a document only for the workspace that uploaded it and converts it on the way to the model.

CREATE TABLE IF NOT EXISTS uploaded_documents (
    id           TEXT PRIMARY KEY,                                  -- tdoc_<uuid>
    workspace_id TEXT NOT NULL,
    media_type   TEXT NOT NULL,
    filename     TEXT NOT NULL DEFAULT '',
    content      BYTEA NOT NULL,
    size_bytes   BIGINT NOT NULL CHECK (size_bytes > 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS uploaded_documents_workspace ON uploaded_documents (workspace_id, created_at DESC);
