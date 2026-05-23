-- Anomaly detection works directly off token_events — no new tables.
-- This composite index makes the per-dimension hourly-aggregation
-- query plan cheap: (workspace_id, team, feature, provider) tuple
-- equality filter + created_at range scan with cost > 0 partial
-- predicate that excludes free-tier rows.

CREATE INDEX IF NOT EXISTS idx_token_events_anomaly
    ON token_events(workspace_id, team, feature, provider, created_at DESC)
    WHERE cost_usd > 0;
