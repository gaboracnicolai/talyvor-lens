-- 0243_tare_training.sql — B27.36: Tare phase 2b learns from Talyvor's synthetic test traffic and from workspaces
-- that opt in, and from nothing else.
--
-- workspaces.tare_training is the opt-in: FALSE for every workspace, existing ones included, until an owner turns it
-- on with PUT /v1/workspaces/{ws}/tare-training. It is its own switch — separate from Sharing (cache_poolable) and
-- from the Tare model (tare_model, 0184) — so turning one on never turns on another, and registration never changes it.
--
-- tare_training_switches is who switched it and when, every time: changed_by is the authenticated caller, and
-- on_behalf_of the person the app names (the signed-in owner), recorded beside it rather than instead of it.
--
-- tare_training_traces is what was collected: the newest message's prose that every phase-1 reducer refused — the
-- text a compressor is trained on. Never a temporary chat's, never a chat's kept out of the shared pool. Switching
-- the opt-in off deletes the workspace's rows in the same transaction, and the training set is read joined on the
-- opt-in as it stands, so a row a lagging replica wrote after the switch is never trained on either.
ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS tare_training BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS tare_training_switches (
    id           BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL CHECK (workspace_id <> ''),
    enabled      BOOLEAN NOT NULL,
    changed_by   TEXT NOT NULL CHECK (changed_by <> ''),
    on_behalf_of TEXT NOT NULL DEFAULT '',
    changed_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS tare_training_switches_ws ON tare_training_switches (workspace_id, changed_at DESC);

CREATE TABLE IF NOT EXISTS tare_training_traces (
    id           BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL CHECK (workspace_id <> ''),
    text         TEXT NOT NULL CHECK (text <> ''),
    text_sha256  TEXT NOT NULL CHECK (text_sha256 ~ '^[0-9a-f]{64}$'),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, text_sha256)
);
