-- DISTILL stage 3 (request-path integration): per-workspace opt-in for
-- document distillation, mirroring logging_policy. Default 'disabled' so the
-- request path stays inert for every existing workspace until an admin enables
-- it via PUT /v1/workspaces/{id}/distill.
ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS distill_policy TEXT NOT NULL DEFAULT 'disabled';
