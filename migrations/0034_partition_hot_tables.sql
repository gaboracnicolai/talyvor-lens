-- 0034_partition_hot_tables.sql
--
-- Hash-partition the three hot-write tables by workspace_id.
--
--   lens_token_ledger — write on every mining / staking / slash event
--   lxc_ledger        — write on every LXC conversion / spend
--   token_events      — write on every AI call (the highest-volume table)
--
-- Without partitioning, each of these is a single heap and a single set
-- of indexes that serialise under concurrent write load. Partitioning
-- distributes inserts across 8 independent storage segments so Postgres
-- can write to multiple segments in parallel.
--
-- PARTITION STRATEGY: HASH(workspace_id), 8 partitions.
--   - workspace_id is NOT NULL in all three tables so no NULL-routing edge
--     cases apply.
--   - Hash gives even distribution regardless of workspace value skew (no
--     hot partition for a common string like 'default').
--   - 8 is a pragmatic starting point for early scale. Postgres hash
--     partitions cannot be split online — this number is a rebuild decision.
--     16 is the next step when write throughput demands it.
--   - Queries that filter on workspace_id hit exactly 1 partition.
--     Queries without a workspace_id filter (analytics) fan out to all 8
--     in parallel — acceptable for off-hot-path aggregations.
--
-- PRIMARY KEY CHANGE: Postgres requires the partition key to appear in
-- every PRIMARY KEY and UNIQUE constraint on a partitioned table. The
-- original single-column PK (id UUID) becomes composite (id, workspace_id).
-- id remains unique within a workspace; all callers already filter by
-- workspace_id before using id, so this change is transparent to queries.
--
-- SAFE TO RUN: no production data exists. The rename → INSERT → DROP
-- sequence is a safety net; the INSERT is a no-op on empty tables.
-- The whole migration runs inside one transaction and rolls back cleanly
-- if anything fails.

BEGIN;

-- ═══════════════════════════════════════════════════════════════════════
-- lens_token_ledger
-- ═══════════════════════════════════════════════════════════════════════

ALTER TABLE lens_token_ledger RENAME TO _lens_token_ledger_unpartitioned;

CREATE TABLE lens_token_ledger (
    id            UUID             NOT NULL DEFAULT gen_random_uuid(),
    workspace_id  TEXT             NOT NULL,
    amount        DOUBLE PRECISION NOT NULL,
    balance_after DOUBLE PRECISION NOT NULL,
    type          TEXT             NOT NULL,
    description   TEXT             NOT NULL DEFAULT '',
    metadata      JSONB            NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, workspace_id)
) PARTITION BY HASH (workspace_id);

CREATE TABLE lens_token_ledger_p0 PARTITION OF lens_token_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 0);
CREATE TABLE lens_token_ledger_p1 PARTITION OF lens_token_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 1);
CREATE TABLE lens_token_ledger_p2 PARTITION OF lens_token_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 2);
CREATE TABLE lens_token_ledger_p3 PARTITION OF lens_token_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 3);
CREATE TABLE lens_token_ledger_p4 PARTITION OF lens_token_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 4);
CREATE TABLE lens_token_ledger_p5 PARTITION OF lens_token_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 5);
CREATE TABLE lens_token_ledger_p6 PARTITION OF lens_token_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 6);
CREATE TABLE lens_token_ledger_p7 PARTITION OF lens_token_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 7);

-- Indexes on the parent table propagate automatically to all 8 partitions.
CREATE INDEX idx_ledger_workspace ON lens_token_ledger (workspace_id, created_at DESC);
CREATE INDEX idx_ledger_type      ON lens_token_ledger (type, workspace_id);

INSERT INTO lens_token_ledger (id, workspace_id, amount, balance_after, type, description, metadata, created_at)
    SELECT id, workspace_id, amount, balance_after, type, description, metadata, created_at
    FROM _lens_token_ledger_unpartitioned;

DROP TABLE _lens_token_ledger_unpartitioned;


-- ═══════════════════════════════════════════════════════════════════════
-- lxc_ledger
-- ═══════════════════════════════════════════════════════════════════════

ALTER TABLE lxc_ledger RENAME TO _lxc_ledger_unpartitioned;

CREATE TABLE lxc_ledger (
    id            UUID             NOT NULL DEFAULT gen_random_uuid(),
    workspace_id  TEXT             NOT NULL,
    amount        DOUBLE PRECISION NOT NULL,
    balance_after DOUBLE PRECISION NOT NULL,
    type          TEXT             NOT NULL,
    description   TEXT             NOT NULL DEFAULT '',
    metadata      JSONB            NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, workspace_id)
) PARTITION BY HASH (workspace_id);

CREATE TABLE lxc_ledger_p0 PARTITION OF lxc_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 0);
CREATE TABLE lxc_ledger_p1 PARTITION OF lxc_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 1);
CREATE TABLE lxc_ledger_p2 PARTITION OF lxc_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 2);
CREATE TABLE lxc_ledger_p3 PARTITION OF lxc_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 3);
CREATE TABLE lxc_ledger_p4 PARTITION OF lxc_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 4);
CREATE TABLE lxc_ledger_p5 PARTITION OF lxc_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 5);
CREATE TABLE lxc_ledger_p6 PARTITION OF lxc_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 6);
CREATE TABLE lxc_ledger_p7 PARTITION OF lxc_ledger FOR VALUES WITH (MODULUS 8, REMAINDER 7);

CREATE INDEX idx_lxc_ledger_workspace ON lxc_ledger (workspace_id, created_at DESC);

INSERT INTO lxc_ledger (id, workspace_id, amount, balance_after, type, description, metadata, created_at)
    SELECT id, workspace_id, amount, balance_after, type, description, metadata, created_at
    FROM _lxc_ledger_unpartitioned;

DROP TABLE _lxc_ledger_unpartitioned;


-- ═══════════════════════════════════════════════════════════════════════
-- token_events
-- ═══════════════════════════════════════════════════════════════════════

ALTER TABLE token_events RENAME TO _token_events_unpartitioned;

-- Column definitions match the cumulative schema across migrations
-- 0001 (base) + 0005 (workspace_id) + 0028 (sprint_id):
--   team / feature / user_id are nullable (no NOT NULL in 0001).
--   created_at has no NOT NULL (matches original).
--   workspace_id DEFAULT 'default' preserved (matches 0005).
CREATE TABLE token_events (
    id            UUID        NOT NULL DEFAULT gen_random_uuid(),
    provider      TEXT        NOT NULL,
    model         TEXT        NOT NULL,
    input_tokens  INTEGER     NOT NULL,
    output_tokens INTEGER     NOT NULL,
    cached        BOOLEAN     NOT NULL DEFAULT false,
    compressed    BOOLEAN     NOT NULL DEFAULT false,
    savings_pct   FLOAT       NOT NULL DEFAULT 0,
    team          TEXT,
    feature       TEXT,
    user_id       TEXT,
    created_at    TIMESTAMPTZ          DEFAULT NOW(),
    prompt_hash   TEXT        NOT NULL DEFAULT '',
    cost_usd      FLOAT       NOT NULL DEFAULT 0,
    pii_detected  BOOLEAN     NOT NULL DEFAULT false,
    workspace_id  TEXT        NOT NULL DEFAULT 'default',
    sprint_id     TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (id, workspace_id)
) PARTITION BY HASH (workspace_id);

CREATE TABLE token_events_p0 PARTITION OF token_events FOR VALUES WITH (MODULUS 8, REMAINDER 0);
CREATE TABLE token_events_p1 PARTITION OF token_events FOR VALUES WITH (MODULUS 8, REMAINDER 1);
CREATE TABLE token_events_p2 PARTITION OF token_events FOR VALUES WITH (MODULUS 8, REMAINDER 2);
CREATE TABLE token_events_p3 PARTITION OF token_events FOR VALUES WITH (MODULUS 8, REMAINDER 3);
CREATE TABLE token_events_p4 PARTITION OF token_events FOR VALUES WITH (MODULUS 8, REMAINDER 4);
CREATE TABLE token_events_p5 PARTITION OF token_events FOR VALUES WITH (MODULUS 8, REMAINDER 5);
CREATE TABLE token_events_p6 PARTITION OF token_events FOR VALUES WITH (MODULUS 8, REMAINDER 6);
CREATE TABLE token_events_p7 PARTITION OF token_events FOR VALUES WITH (MODULUS 8, REMAINDER 7);

CREATE INDEX idx_token_events_created      ON token_events (created_at DESC);
CREATE INDEX idx_token_events_prompt_hash  ON token_events (prompt_hash);
CREATE INDEX idx_token_events_workspace    ON token_events (workspace_id, created_at DESC);
CREATE INDEX idx_token_events_budget_scope ON token_events (workspace_id, team, sprint_id, created_at DESC);

INSERT INTO token_events (
    id, provider, model, input_tokens, output_tokens, cached, compressed,
    savings_pct, team, feature, user_id, created_at, prompt_hash, cost_usd,
    pii_detected, workspace_id, sprint_id
)
    SELECT
        id, provider, model, input_tokens, output_tokens, cached, compressed,
        savings_pct, team, feature, user_id, created_at, prompt_hash, cost_usd,
        pii_detected, workspace_id, sprint_id
    FROM _token_events_unpartitioned;

DROP TABLE _token_events_unpartitioned;

COMMIT;
