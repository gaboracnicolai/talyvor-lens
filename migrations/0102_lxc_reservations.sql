-- 0102_lxc_reservations.sql — the agent-allocation RESERVATION substrate (billing redesign).
--
-- Replaces the permanent pre-serve debit (SpendLXCForAgent) with a HOLD → SETTLE/RELEASE lifecycle
-- so the customer is billed what was actually DELIVERED, not a pre-serve estimate. MONEY-adjacent
-- but MINTS NOTHING: like 0079, it bounds/records SPENDING of already-existing LXC. CREATE-only,
-- additive — no ALTER/DROP on lxc_balances / lxc_ledger / any existing economy table (so it composes
-- with the append-only trigger from 0055, which forbids UPDATE/DELETE on lxc_ledger — every balance
-- move here is a NEW lxc_ledger row, never an edit).
--
-- WHY A NEW TABLE (not lxc_ledger status): lxc_ledger is append-only+immutable (0055 audit_no_mutation
-- trigger). A held→settled→released state transition needs MUTABLE status, which the ledger structurally
-- cannot carry. lxc_reservations holds ONLY the reservation lifecycle state; every actual balance
-- movement is an immutable lxc_ledger row (hold / release / spend), keyed back by reservation_id via
-- the request_id join. lxc_reservations is deliberately NOT in the 0055 guarded set — it is billing
-- STATE (legitimately transitioned), not the audit trail (which is the ledger rows it stamps).

CREATE TABLE IF NOT EXISTS lxc_reservations (
    -- The server-derived debit key (hex(SHA256(salt ‖ apiKeyID ‖ nonce)); agent_allocator.go). PRIMARY KEY
    -- = exactly-once on the HOLD: a replayed reserve INSERTs 0 rows (ON CONFLICT DO NOTHING) and debits nothing.
    reservation_id  TEXT        PRIMARY KEY,
    scoped_key_id   TEXT        NOT NULL,            -- the agent whose sub-budget this holds against
    workspace_id    TEXT        NOT NULL,            -- the funding workspace (lxc_balances)
    held_ulxc       BIGINT      NOT NULL,            -- the CONSERVATIVE (output-aware) hold amount, µLXC
    settled_ulxc    BIGINT,                          -- the actual DELIVERED charge on settle, µLXC; NULL until resolved
    status          TEXT        NOT NULL DEFAULT 'held'
                    CHECK (status IN ('held', 'settled', 'released')),
    requested_model TEXT,                            -- AgentDebitMeta: model the hold was estimated on (non-content)
    request_id      TEXT,                            -- AgentDebitMeta: token_events join (served model + real tokens)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ                      -- when settled or released
);

-- Sweeper index: find stranded holds (a crash between reserve and settle) to RELEASE — a crash must
-- REFUND the customer, never auto-settle. Partial index keeps it tiny (only open holds).
CREATE INDEX IF NOT EXISTS idx_lxc_reservations_stranded
    ON lxc_reservations (created_at)
    WHERE status = 'held';
