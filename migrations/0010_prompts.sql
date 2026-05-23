CREATE TABLE IF NOT EXISTS prompts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1,
    content      TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    workspace_id TEXT NOT NULL DEFAULT 'default',
    is_active    BOOLEAN NOT NULL DEFAULT true,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ DEFAULT NOW(),
    updated_at   TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(name, version, workspace_id)
);

CREATE INDEX IF NOT EXISTS idx_prompts_active
    ON prompts(workspace_id, name)
    WHERE is_active = true;

CREATE INDEX IF NOT EXISTS idx_prompts_history
    ON prompts(name, workspace_id, version DESC);
