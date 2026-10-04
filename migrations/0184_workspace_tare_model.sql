-- 0184_workspace_tare_model.sql — B27.35: Tare phase 2a, the compression model, is OFF unless the
-- workspace opts in.
--
-- tare_policy (0126) switches Tare as a whole, and it defaults to ON (B8.3) because phase 1 is
-- lossless for JSON and logs and announces its one lossy cut. The phase 2a model DROPS WORDS from
-- prose, so it gets its own switch with the opposite default: FALSE for every workspace, existing ones
-- included. It runs only when tare_policy lets Tare run AND this is true. Registration never changes
-- it (like the consent columns); PUT /v1/workspaces/{ws}/tare-model is the only writer.
ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS tare_model BOOLEAN NOT NULL DEFAULT false;
