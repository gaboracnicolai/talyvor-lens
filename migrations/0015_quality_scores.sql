-- Per-call quality scores so the new CompositeScorer can report
-- workspace-level distributions (avg, p50, p75, p95, low-quality
-- count). Kept as an append-only event table — analytics queries
-- live on the read side.
--
-- workspace_id is text rather than uuid to match the other
-- workspace-keyed tables in this repo (token_events, alerts).

CREATE TABLE IF NOT EXISTS quality_scores (
    id          BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    score       DOUBLE PRECISION NOT NULL,
    provider    TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',
    feature     TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Workspace + recency hits both common query shapes:
-- "stats for ws-X over the last 24h" and "low-quality recent
-- entries for the dashboard".
CREATE INDEX IF NOT EXISTS idx_quality_scores_ws_time
    ON quality_scores(workspace_id, created_at DESC);
