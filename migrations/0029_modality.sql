-- 0029_modality.sql — vision / multimodal routing (Master Plan Upgrade 15).
--
-- Records the modality of each billed request on the SAME single billing
-- write (alerts.RecordSpend → token_events), so budgets, forecasting,
-- anomaly detection, and the ROI report can reflect image/audio cost rather
-- than treating every request as text.
--
--   modality       — canonical modality label for the request:
--                    'text' (default), or a comma-joined set of the
--                    non-text modalities present, e.g. 'image',
--                    'image,audio'. Bounded, low-cardinality.
--   cost_estimated — TRUE when the cost is a documented estimate rather
--                    than exact accounting. Multimodal input-token counts
--                    are estimated (text chars + a per-image estimate)
--                    because exact image-token accounting needs the
--                    provider's returned usage; this flags those rows as
--                    estimates so downstream consumers don't over-trust them.
--
-- Both columns default to the text/exact case, so historical rows and the
-- existing text-only write path are unaffected.

ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS modality TEXT NOT NULL DEFAULT 'text';

ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS cost_estimated BOOLEAN NOT NULL DEFAULT FALSE;

-- Multimodal rows are a small fraction of traffic; a partial index keeps
-- "show me image-heavy spend" queries cheap without bloating the common path.
CREATE INDEX IF NOT EXISTS idx_token_events_modality
  ON token_events(workspace_id, modality, created_at DESC)
  WHERE modality <> 'text';
