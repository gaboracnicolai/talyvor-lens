-- 0056_audit_export_state.sql — U14 off-box export watermark.
--
-- A single-row table holding the high-water mark: the created_at upper bound of
-- the last SUCCESSFULLY off-box-exported window. The leader-elected export loop
-- (internal/audit/scheduled_export.go) reads it, exports token_events in the
-- window (watermark, now], and advances it to `now` ONLY when the sink POST
-- succeeds. On failure the watermark does NOT advance, so the next run re-exports
-- the same window — AT-LEAST-ONCE delivery: no audit row is ever lost, at the cost
-- of a possible boundary-row duplicate (SIEMs dedup on request_id).
--
-- Mutable by design — deliberately NOT in the 0055 append-only set (the watermark
-- advances on every successful export).
CREATE TABLE IF NOT EXISTS audit_export_state (
    id               BOOLEAN     PRIMARY KEY DEFAULT true,
    last_exported_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT audit_export_state_singleton CHECK (id)
);

-- Seed the single row so the loop can UPDATE it without an upsert.
INSERT INTO audit_export_state (id) VALUES (true) ON CONFLICT (id) DO NOTHING;
