-- 0068_proof_of_benchmark.sql — proof-of-benchmark MEASUREMENT (P1 #10, PR-A).
--
-- Three ADDITIVE tables; NO alters to any ledger/money table. The per-node score is RECORDED but
-- feeds NO routing and NO mint in this PR. Live /inference delivery + the receipt-mint suppression
-- land in PR-A.5; routing consumption is PR-B.
--
-- Trust model: benchmark_eval_items is VERIFIER-PRIVATE — NO workspace_id (not tenant-owned). The
-- expected_output is held verifier-side and is NEVER sent to a node (a node receives only `input`).

-- The verifier-private, rotating eval pool.
CREATE TABLE IF NOT EXISTS benchmark_eval_items (
    id              TEXT PRIMARY KEY,
    input           TEXT NOT NULL,
    expected_output TEXT NOT NULL,
    eval_method     TEXT NOT NULL DEFAULT 'exact',   -- exact | contains | regex | json_schema (eval.StaticScore)
    pass_threshold  DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at      TIMESTAMPTZ
);

-- The descriptive per-node quality signal (read by NOTHING in PR-A).
CREATE TABLE IF NOT EXISTS benchmark_node_scores (
    node_id      TEXT NOT NULL,
    model        TEXT NOT NULL,
    score        DOUBLE PRECISION NOT NULL DEFAULT 0,
    sample_count INTEGER NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (node_id, model)
);

-- The never-reuse ledger + audit. UNIQUE(node_id, item_id) enforces that an item is probed to a
-- given node AT MOST ONCE (anti-gaming invariant 1). request_id is populated when live delivery
-- lands (PR-A.5); unused this PR.
CREATE TABLE IF NOT EXISTS benchmark_probes (
    id         TEXT PRIMARY KEY,
    node_id    TEXT NOT NULL,
    item_id    TEXT NOT NULL,
    request_id TEXT,
    served_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    score      DOUBLE PRECISION NOT NULL DEFAULT 0,
    UNIQUE (node_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_benchmark_probes_request ON benchmark_probes (request_id);
CREATE INDEX IF NOT EXISTS idx_benchmark_eval_items_active ON benchmark_eval_items (active) WHERE active;
