-- 0126_tare_metering.sql — B6.4/B6.5: the Tare metering record, and the workspace switch for it.
--
-- ON THE SEAM THAT ALREADY METERS. Tare's record rides the request's own billing write
-- (alerts.RecordSpend → token_events), the same single write distill_method (0040) tags — never a
-- second row. A row Tare did not change keeps every column at its default:
--
--   tare_kind           — which reducer shrank the newest message ('json' / 'code' / 'log');
--                         '' = Tare did not change this request.
--   tare_tokens_in/out  — the request body's token ESTIMATE (len/4, the repo's convention) before
--                         and after the reduction. Estimates, not provider counts; the provider's
--                         own count of what it billed is input_tokens on the same row.
--   tare_delta_cost_usd — (tokens_in − tokens_out) priced at the billed model's INPUT rate.
--   tare_work_item_id   — X-Talyvor-Issue: the work item the caller declared. ⚠ CALLER-DECLARED,
--                         like request_attribution.issue_id; docs/tare-phase1d-brief.md measured
--                         that no attribution header is signed. It labels a customer's own saving
--                         for display. It is NOT an economy input and nothing mints from it.
--
-- ⚠ NOT savings_pct. That column is tombstoned and writerless (0114); these are new columns with
-- their writer (alerts.recordSpend) and reader (alerts.TareSavings) in the same merge.
ALTER TABLE token_events
  ADD COLUMN IF NOT EXISTS tare_kind           TEXT             NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS tare_tokens_in      INTEGER          NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS tare_tokens_out     INTEGER          NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS tare_delta_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS tare_work_item_id   TEXT             NOT NULL DEFAULT '';

-- Reduced rows are a small fraction of traffic; the reader groups them by work item.
CREATE INDEX IF NOT EXISTS idx_token_events_tare
  ON token_events(workspace_id, tare_work_item_id)
  WHERE tare_kind <> '';

-- The switch. Mirrors distill_policy (0039) and compression_policy (0117): a stored policy plus a
-- per-request X-Talyvor-Tare header. DISABLED for every workspace, existing ones included — the
-- code trimmer is LOSSY (it drops function bodies, announced), so a workspace turns it on itself.
ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS tare_policy TEXT NOT NULL DEFAULT 'disabled';
