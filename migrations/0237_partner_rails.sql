-- B37.2 — each money rail's last answer, kept so a restart and every Lens process show it.
--
-- One row per partner service (partners.Services), updated in place: when its partner last answered and last failed,
-- from a real call or the 5-minute probe. Each write keeps the later of what is stored and what the process saw.
-- It holds no reference, key, error text or anything about a workspace.

CREATE TABLE IF NOT EXISTS partner_rails (
    service      TEXT PRIMARY KEY,
    last_success TIMESTAMPTZ,
    last_failure TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
