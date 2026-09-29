-- 0169_answer_removals.sql — B23.1: a thumbs-down removes a wrong answer.
--
-- One row per thumbs-down (POST /v1/feedback, signal negative or repeat, on a request id): who marked
-- it, when, where the answer had been served from ('fresh' when it was the model's own answer, else
-- the cache layer), and how many stored rows and exact-cache copies were removed. It holds no
-- question or answer, and is kept when a workspace leaves, as the record that the answer was removed.

CREATE TABLE IF NOT EXISTS answer_removals (
  id                   BIGSERIAL PRIMARY KEY,
  workspace_id         TEXT        NOT NULL,
  request_id           TEXT        NOT NULL,
  signal               TEXT        NOT NULL CHECK (signal IN ('negative', 'repeat')),
  served_from          TEXT        NOT NULL DEFAULT '',
  marked_by            TEXT        NOT NULL,
  answers_removed      INTEGER     NOT NULL DEFAULT 0,
  exact_copies_removed INTEGER     NOT NULL DEFAULT 0,
  marked_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_answer_removals_workspace ON answer_removals (workspace_id, marked_at DESC);
