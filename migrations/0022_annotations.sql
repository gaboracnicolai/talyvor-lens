-- 0022_annotations.sql — quality-annotation mining (Batch 2 Item 4).
--
-- Three tables:
--   annotation_tasks  — pairwise/rating jobs to be reviewed.
--                        Stripped of all PII before insertion.
--   annotations       — each reviewer's verdict for a task.
--                        UNIQUE(task_id, annotator_id) prevents
--                        a workspace from voting twice on the
--                        same task.
--   annotator_stakes  — LENS locked up to grant annotation rights.

CREATE TABLE IF NOT EXISTS annotation_tasks (
    id               TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    source_workspace TEXT NOT NULL,
    prompt_hash      TEXT NOT NULL,
    response_a       TEXT NOT NULL,
    response_b       TEXT NOT NULL,
    task_type        TEXT NOT NULL DEFAULT 'pairwise',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at       TIMESTAMPTZ NOT NULL
);

-- Partial index — only "still-open" tasks are interesting for
-- the GetPendingTask hot path. PG re-evaluates the WHERE clause
-- automatically on each query so we never have to compact the
-- index ourselves.
CREATE INDEX IF NOT EXISTS idx_tasks_pending
    ON annotation_tasks (expires_at)
    WHERE expires_at > NOW();

CREATE INDEX IF NOT EXISTS idx_tasks_source_workspace
    ON annotation_tasks (source_workspace);

CREATE TABLE IF NOT EXISTS annotations (
    id            TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    task_id       TEXT NOT NULL REFERENCES annotation_tasks(id) ON DELETE CASCADE,
    annotator_id  TEXT NOT NULL,
    decision      TEXT NOT NULL,
    confidence    INTEGER NOT NULL DEFAULT 3 CHECK (confidence BETWEEN 1 AND 5),
    time_spent_ms INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (task_id, annotator_id)
);

CREATE INDEX IF NOT EXISTS idx_annotations_task ON annotations (task_id);
CREATE INDEX IF NOT EXISTS idx_annotations_annotator ON annotations (annotator_id, created_at DESC);

CREATE TABLE IF NOT EXISTS annotator_stakes (
    workspace_id TEXT PRIMARY KEY,
    staked       DOUBLE PRECISION NOT NULL DEFAULT 0,
    staked_at    TIMESTAMPTZ
);
