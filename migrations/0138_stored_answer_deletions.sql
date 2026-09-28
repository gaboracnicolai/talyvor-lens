-- 0138_stored_answer_deletions.sql — B21.3: a workspace can delete its stored answers, or ask Talyvor
-- to delete everything it holds for that user or company.
--
-- deletion_requests is the "on request" path: who asked, when, and whether an operator has done it.
-- stored_answer_deletions is the audit log of every deletion actually run — by the workspace itself
-- (DELETE /v1/workspaces/{ws}/stored-answers) or by an operator completing a request — with what it
-- removed. Neither holds any question or answer; both are kept when a workspace leaves, as the record
-- that its data was deleted.

CREATE TABLE IF NOT EXISTS deletion_requests (
  id           BIGSERIAL PRIMARY KEY,
  workspace_id TEXT        NOT NULL,
  requested_by TEXT        NOT NULL,
  note         TEXT        NOT NULL DEFAULT '',
  status       TEXT        NOT NULL DEFAULT 'requested' CHECK (status IN ('requested', 'done')),
  requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  completed_at TIMESTAMPTZ,
  completed_by TEXT
);

CREATE INDEX IF NOT EXISTS idx_deletion_requests_workspace ON deletion_requests (workspace_id, requested_at DESC);
CREATE INDEX IF NOT EXISTS idx_deletion_requests_open ON deletion_requests (requested_at) WHERE status = 'requested';

CREATE TABLE IF NOT EXISTS stored_answer_deletions (
  id                  BIGSERIAL PRIMARY KEY,
  workspace_id        TEXT        NOT NULL,
  scope               TEXT        NOT NULL CHECK (scope IN ('shared', 'all')),
  deleted_by          TEXT        NOT NULL,
  deletion_request_id BIGINT      REFERENCES deletion_requests (id),
  shared_answers      BIGINT      NOT NULL,
  private_answers     BIGINT      NOT NULL,
  shared_conversions  BIGINT      NOT NULL,
  private_conversions BIGINT      NOT NULL,
  cached_copies       BIGINT      NOT NULL,
  deleted_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_stored_answer_deletions_workspace ON stored_answer_deletions (workspace_id, deleted_at DESC);
