-- 0036_check_constraints.sql
--
-- Adds DB-level CHECK constraints that enforce non-negativity on every
-- financial column. Application code already prevents negative balances, but
-- a DB constraint is a second line of defence: if a bug slips through the
-- debit path, Postgres rejects the write rather than silently persisting a
-- corrupted state.
--
-- Uses NOT VALID so the constraint is enforced on NEW writes immediately
-- without a full table scan that would lock the table. Run
--   VALIDATE CONSTRAINT <name> ON <table>
-- during a maintenance window to back-fill validation of existing rows.

-- ── lens_token_balances ────────────────────────────────────────────────────
ALTER TABLE lens_token_balances
    ADD CONSTRAINT chk_token_balance_gte_zero
        CHECK (balance >= 0) NOT VALID;

ALTER TABLE lens_token_balances
    ADD CONSTRAINT chk_token_locked_balance_gte_zero
        CHECK (locked_balance >= 0) NOT VALID;

ALTER TABLE lens_token_balances
    ADD CONSTRAINT chk_token_lifetime_earned_gte_zero
        CHECK (lifetime_earned >= 0) NOT VALID;

ALTER TABLE lens_token_balances
    ADD CONSTRAINT chk_token_lifetime_spent_gte_zero
        CHECK (lifetime_spent >= 0) NOT VALID;

-- ── lens_token_ledger ──────────────────────────────────────────────────────
-- amount CAN be negative (debits are recorded as negative values).
-- balance_after is the running balance snapshot and must always be ≥ 0.
ALTER TABLE lens_token_ledger
    ADD CONSTRAINT chk_ledger_balance_after_gte_zero
        CHECK (balance_after >= 0) NOT VALID;

-- ── povi_stakes ────────────────────────────────────────────────────────────
ALTER TABLE povi_stakes
    ADD CONSTRAINT chk_povi_stake_amount_gte_zero
        CHECK (amount >= 0) NOT VALID;

ALTER TABLE povi_stakes
    ADD CONSTRAINT chk_povi_slashed_amount_gte_zero
        CHECK (slashed_amount >= 0) NOT VALID;
