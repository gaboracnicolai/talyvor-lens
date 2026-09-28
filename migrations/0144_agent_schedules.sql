-- B19.8 — scheduled payments and automatic top-ups.
--
-- A schedule pays another of the workspace's agents every hour, day, week or month. Each tick runs
-- exactly once: its run row (schedule_id, tick_at) and its payment's postings are written, and
-- next_run_at advanced, in ONE transaction that holds the schedule's row, so neither a restart nor a
-- second Lens running the same tick can pay it twice. A tick the payer's rules or balance refuse is
-- still a run, with outcome 'refused' and why.
CREATE TABLE IF NOT EXISTS agent_payment_schedules (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    from_agent_id TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    to_agent_id   TEXT NOT NULL REFERENCES agent_accounts (id) ON DELETE CASCADE,
    amount_ulxc   BIGINT NOT NULL CHECK (amount_ulxc > 0),
    memo          TEXT NOT NULL DEFAULT '',
    every         TEXT NOT NULL CHECK (every IN ('hour', 'day', 'week', 'month')),
    next_run_at   TIMESTAMPTZ NOT NULL,
    active        BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_agent_id <> to_agent_id)
);
CREATE INDEX IF NOT EXISTS idx_agent_payment_schedules_due ON agent_payment_schedules (next_run_at) WHERE active;
CREATE INDEX IF NOT EXISTS idx_agent_payment_schedules_workspace ON agent_payment_schedules (workspace_id);

CREATE TABLE IF NOT EXISTS agent_schedule_runs (
    schedule_id TEXT NOT NULL REFERENCES agent_payment_schedules (id) ON DELETE CASCADE,
    tick_at     TIMESTAMPTZ NOT NULL,
    outcome     TEXT NOT NULL CHECK (outcome IN ('paid', 'refused')),
    entry_id    UUID,                    -- the payment's agent_postings entry, when paid
    detail      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (schedule_id, tick_at)
);

-- An agent's automatic top-up: when its balance is below below_ulxc, the workspace's unallocated LXC
-- brings it back up to to_ulxc (as much of it as there is), posted as a 'topup' entry.
CREATE TABLE IF NOT EXISTS agent_topups (
    agent_id     TEXT PRIMARY KEY REFERENCES agent_accounts (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    below_ulxc   BIGINT NOT NULL CHECK (below_ulxc > 0),
    to_ulxc      BIGINT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (to_ulxc > below_ulxc)
);
CREATE INDEX IF NOT EXISTS idx_agent_topups_workspace ON agent_topups (workspace_id);
