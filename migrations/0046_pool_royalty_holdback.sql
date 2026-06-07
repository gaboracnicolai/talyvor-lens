-- 0046_pool_royalty_holdback.sql — Phase-2 Stage 2.3a: the holdback/finality
-- ledger for Pool-B royalty mints.
--
-- A pool-royalty mint now credits a HELD balance instead of spendable: the
-- LENS sits in held_balance for a configurable window (default 72h —
-- comfortably above the hours-scale detection latency of the statistical
-- gaming vectors, whose evidence lives on the durable claim rows), then the
-- leader-elected finalize sweeper settles held → spendable. A confirmed-bad
-- mint is revoked by burning from held (it never entered circulating supply,
-- so revoking must not decrease it either). Supply counts at FINALIZE: the
-- counted ledger type ('pool_royalty', already in GetTotalSupply's
-- allow-list since Stage 2.2) is now WRITTEN by the finalize event, while
-- the mint writes 'pool_royalty_held' and a revoke writes
-- 'pool_royalty_revoked' — both uncounted by omission, the same mechanism
-- that excludes receipt_mine_provisional and marketplace_fee. No supply
-- reader changes at all.
--
--   1. lens_token_balances.held_balance — unfinalized royalty income, a NEW
--      column deliberately SEPARATE from locked_balance: locked is staking
--      COLLATERAL (0032 — the operator's own LENS held hostage), held is
--      not-yet-final INCOME. Different semantics, different slash
--      destinations, different owners' code. ADDITIVE and default 0 — every
--      existing path (Credit/Debit/Transfer/Burn/stakes) ignores it. Moved
--      held↔spendable↔revoked ONLY by the atomic
--      CreditHeldTx/FinalizeHeldTx/RevokeHeldTx kernel (held_ledger.go),
--      which copies the stakeInner PATTERN over this column and touches no
--      stake function.
--
--   2. pool_royalty_mints.status + finalize_after — the per-mint lifecycle
--      (held | final | revoked) and its settlement deadline. DEFAULT 'final'
--      correctly grandfathers every pre-2.3a row (those were minted straight
--      to spendable and already supply-counted at mint). finalize_after is
--      NULL on final rows. Transitions are CAS UPDATEs guarded by
--      RowsAffected (the povi_challenges / 2.1-claim idiom), so overlapping
--      HA sweepers can never double-finalize.
--
-- The settlement TRIGGER is deliberately decoupled from the ledger ops: the
-- timed sweeper is the initial trigger; billing settlement can replace it
-- later without touching this schema or the kernel.
--
-- Additive, idempotent, own-file. The 0036-style CHECK is NOT VALID so it
-- enforces on new writes without a table-scan lock.

ALTER TABLE lens_token_balances
    ADD COLUMN IF NOT EXISTS held_balance DOUBLE PRECISION NOT NULL DEFAULT 0;

ALTER TABLE lens_token_balances
    DROP CONSTRAINT IF EXISTS chk_token_held_balance_gte_zero;
ALTER TABLE lens_token_balances
    ADD CONSTRAINT chk_token_held_balance_gte_zero
        CHECK (held_balance >= 0) NOT VALID;

ALTER TABLE pool_royalty_mints
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'final';

ALTER TABLE pool_royalty_mints
    ADD COLUMN IF NOT EXISTS finalize_after TIMESTAMPTZ;

-- Held rows are a small transient fraction; the partial index keeps the
-- sweep (WHERE status='held' AND finalize_after < now()) cheap without
-- bloating the common path (the 0029/0042 partial-index idiom).
CREATE INDEX IF NOT EXISTS idx_pool_royalty_mints_finalize
    ON pool_royalty_mints (finalize_after)
    WHERE status = 'held';

-- Stage 2.3a makes the margin view STATUS-AWARE: realized margin must count
-- FINAL rows only (held = pending, may yet be revoked; revoked = burned,
-- fraudulent attribution — counting either overstates Talyvor's realized
-- (1−s) margin). CREATE OR REPLACE appends the status column (Postgres
-- permits appending; existing columns keep name/type/order) and the
-- MarginReader filters status='final'. The view itself keeps ALL rows so
-- forensics can query held/revoked populations.
CREATE OR REPLACE VIEW pool_royalty_margin AS
SELECT
    request_id,
    requester_workspace_id,
    contributor_workspace_id,
    layer,
    provider,
    model,
    avoided_cogs_usd,
    minted_amount,
    avoided_cogs_usd - minted_amount AS margin_usd,
    created_at,
    status
FROM pool_royalty_mints;
