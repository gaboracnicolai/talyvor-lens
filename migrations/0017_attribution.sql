-- request_attribution captures full Git + workspace context per
-- AI call. Coexists with branch_spend (migration 0004) which
-- powers the original Tracker — the two tables serve different
-- read patterns:
--
--   - branch_spend: per-branch rollup keyed on (repo, branch).
--   - request_attribution: per-request audit row keyed on
--     workspace_id with Git context as additive columns.
--
-- Every column except workspace_id defaults to '' / 0 so a
-- request without Git headers still inserts cleanly.

CREATE TABLE IF NOT EXISTS request_attribution (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  TEXT NOT NULL,
    feature       TEXT NOT NULL DEFAULT '',
    issue_id      TEXT NOT NULL DEFAULT '',
    branch        TEXT NOT NULL DEFAULT '',
    pr_number     TEXT NOT NULL DEFAULT '',
    commit_sha    TEXT NOT NULL DEFAULT '',
    author        TEXT NOT NULL DEFAULT '',
    repo_name     TEXT NOT NULL DEFAULT '',
    user_id       TEXT NOT NULL DEFAULT '',
    session_id    TEXT NOT NULL DEFAULT '',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd      DOUBLE PRECISION NOT NULL DEFAULT 0,
    model         TEXT NOT NULL DEFAULT '',
    provider      TEXT NOT NULL DEFAULT '',
    latency_ms    INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_attribution_workspace
    ON request_attribution(workspace_id, created_at DESC);

-- Partial indexes for the three lookup-by-Git-dimension paths.
-- These keep the index small (most rows skip them) and the
-- per-dimension queries fast.
CREATE INDEX IF NOT EXISTS idx_attribution_branch
    ON request_attribution(workspace_id, branch)
    WHERE branch != '';

CREATE INDEX IF NOT EXISTS idx_attribution_pr
    ON request_attribution(workspace_id, pr_number)
    WHERE pr_number != '';

CREATE INDEX IF NOT EXISTS idx_attribution_issue
    ON request_attribution(workspace_id, issue_id)
    WHERE issue_id != '';
