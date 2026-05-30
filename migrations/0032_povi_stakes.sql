-- 0032_povi_stakes.sql — node-registration staking (PoVI Token Economy Phase 1,
-- Part 2 / Master Plan Upgrade 6).
--
-- Sybil resistance for PoVI: a node must lock LENS collateral to become
-- minting-eligible, and that collateral is slashable (Part 3) — so getting
-- caught cheating costs real value rather than a free re-registration.
--
-- Two changes:
--
--   1. lens_token_balances.locked_balance — staking is COLLATERAL, not payment:
--      locked LENS is still the operator's (owned), just held hostage. So it is
--      a real balance STATE (available vs locked-but-owned), moved available↔
--      locked↔burned by the atomic LedgerStore.LockStake/ReleaseStake/SlashStake
--      operations. ADDITIVE and default 0 — the existing Credit/Debit/Transfer/
--      Burn paths (which model PAYMENT, where money genuinely leaves) are
--      UNCHANGED and ignore this column.
--
--   2. povi_stakes — one row per node: the collateral, its lifecycle status,
--      and the unbonding timer. Slashable while active OR unbonding (the
--      anti-yank property: a node can't cheat then instantly withdraw before a
--      challenge slashes it).

ALTER TABLE lens_token_balances
    ADD COLUMN IF NOT EXISTS locked_balance DOUBLE PRECISION NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS povi_stakes (
    node_id        TEXT PRIMARY KEY,
    workspace_id   TEXT NOT NULL,
    amount         DOUBLE PRECISION NOT NULL DEFAULT 0,  -- current locked collateral
    status         TEXT NOT NULL DEFAULT 'active',       -- active | unbonding | released | slashed
    slashed_amount DOUBLE PRECISION NOT NULL DEFAULT 0,  -- cumulative slashed (audit)
    locked_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    unbond_at      TIMESTAMPTZ,                          -- set when unbonding begins; release allowed after
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_povi_stakes_workspace ON povi_stakes(workspace_id);
CREATE INDEX IF NOT EXISTS idx_povi_stakes_status ON povi_stakes(status);
