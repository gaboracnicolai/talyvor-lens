-- 0040_distill_method.sql — DISTILL per-event attribution (stage 3, PR #4).
--
-- Makes the DISTILL story DURABLE and AUDITABLE per request, not just an
-- aggregate Prometheus metric. Each billed spend row records HOW (if at all)
-- the request was distilled, on the SAME single billing write
-- (alerts.RecordSpend → token_events):
--
--   distill_method — how this row's spend relates to distillation:
--                    ''           — not distilled (the default; every existing
--                                   and non-distill row, so historical rows and
--                                   the normal text path are unaffected).
--                    'convert'    — the request's prompt was distilled to clean
--                                   Markdown before the model saw it; the saving
--                                   is IMPLICIT in this row's (lower) input_tokens
--                                   — there is no separate saving write.
--                    'vision_ocr' — this row is the OCR sub-call's OWN cost (a
--                                   text-less document recovered via a vision
--                                   model). A COST, never a saving, never blended
--                                   into the 'convert' row. cost_estimated is TRUE
--                                   on these rows (document/image token accounting
--                                   is inherently approximate).
--
-- The net DISTILL value per workspace is then auditable directly from
-- token_events: SUM(saving implicit in 'convert' rows) − SUM(cost of
-- 'vision_ocr' rows) — which may be net-negative, and that surfaces honestly.
--
-- Bounded, low-cardinality (three values). Defaults to '' so the column is inert
-- for all existing traffic.

ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS distill_method TEXT NOT NULL DEFAULT '';

-- Distilled rows are a small fraction of traffic; a partial index keeps
-- "show me distilled spend / OCR cost" queries cheap without bloating the
-- common (non-distilled) path.
CREATE INDEX IF NOT EXISTS idx_token_events_distill_method
  ON token_events(workspace_id, distill_method, created_at DESC)
  WHERE distill_method <> '';
