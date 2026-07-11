-- 0081_sec2_lens_integer_ulens.sql
--
-- SEC-2 (WATCHED money-path change): convert every CONSERVED LENS amount from
-- DOUBLE PRECISION to BIGINT counting the token's SMALLEST UNIT — the micro-LENS
-- (µLENS): 1 LENS = 1_000_000 µLENS. Float money is non-exact and non-associative,
-- so the float ledger did not conserve value (a ~3.33e-7 LENS leak per trade,
-- proven RED in internal/economy/sec2_float_drift_test.go). Integer µLENS makes
-- ledger arithmetic EXACT and conservation hold to the unit.
--
-- The DB was dropped and no rows exist, so this is a schema + code-correctness
-- change, not a data migration — but it is written as a proper forward migration:
-- `USING (round(col * 1000000))::bigint` is the correct whole-LENS → µLENS
-- conversion had any rows existed. The non-negativity CHECK constraints (0036)
-- carry over unchanged; ledger `amount` may be negative (debits) and is not
-- constrained.
--
-- Tier-2 (rates: apy, conversion rate, price_per_token) and Tier-3 (USD:
-- price_usd, min_buy_usd, avoided_cogs_usd, similarity, scores) stay DOUBLE — see
-- the SEC-2 report for the deferred Tier-2/3 follow-up.
--
-- lens_token_ledger and lxc_ledger are PARTITIONED (0034); altering the
-- partitioned parent propagates the type change to every hash partition.

BEGIN;

-- ── canonical LENS ledger (partitioned) ─────────────────────────────────────
ALTER TABLE lens_token_ledger
    ALTER COLUMN amount        TYPE BIGINT USING (round(amount * 1000000))::bigint,
    ALTER COLUMN balance_after TYPE BIGINT USING (round(balance_after * 1000000))::bigint;

-- ── LENS balances (0019 + locked_balance 0032 + held_balance 0046) ───────────
ALTER TABLE lens_token_balances
    ALTER COLUMN balance         TYPE BIGINT USING (round(balance * 1000000))::bigint,
    ALTER COLUMN lifetime_earned TYPE BIGINT USING (round(lifetime_earned * 1000000))::bigint,
    ALTER COLUMN lifetime_spent  TYPE BIGINT USING (round(lifetime_spent * 1000000))::bigint,
    ALTER COLUMN locked_balance  TYPE BIGINT USING (round(locked_balance * 1000000))::bigint,
    ALTER COLUMN held_balance    TYPE BIGINT USING (round(held_balance * 1000000))::bigint;

-- ── annotator collateral (0022) ─────────────────────────────────────────────
ALTER TABLE annotator_stakes
    ALTER COLUMN staked TYPE BIGINT USING (round(staked * 1000000))::bigint;

-- ── marketplace (0024): amounts + fee are LENS; price_usd/min_buy_usd/apy stay float
ALTER TABLE marketplace_listings
    ALTER COLUMN amount TYPE BIGINT USING (round(amount * 1000000))::bigint;
ALTER TABLE marketplace_trades
    ALTER COLUMN amount      TYPE BIGINT USING (round(amount * 1000000))::bigint,
    ALTER COLUMN talyvor_fee TYPE BIGINT USING (round(talyvor_fee * 1000000))::bigint;
ALTER TABLE stake_positions
    ALTER COLUMN amount TYPE BIGINT USING (round(amount * 1000000))::bigint;

-- ── PoVI node staking (0032) + challenges (0033) ────────────────────────────
ALTER TABLE povi_stakes
    ALTER COLUMN amount         TYPE BIGINT USING (round(amount * 1000000))::bigint,
    ALTER COLUMN slashed_amount TYPE BIGINT USING (round(slashed_amount * 1000000))::bigint;
ALTER TABLE povi_challenges
    ALTER COLUMN slashed_amount TYPE BIGINT USING (round(slashed_amount * 1000000))::bigint;

-- ── mining earnings (routing_patterns 0023, pattern_mine_credits 0049) ───────
ALTER TABLE routing_patterns
    ALTER COLUMN earned TYPE BIGINT USING (round(earned * 1000000))::bigint;
ALTER TABLE pattern_mine_credits
    ALTER COLUMN earned TYPE BIGINT USING (round(earned * 1000000))::bigint;

-- ── idempotent-mint audit (0057): amount is the credited LENS ───────────────
ALTER TABLE mint_idempotency
    ALTER COLUMN amount TYPE BIGINT USING (round(amount * 1000000))::bigint;

-- ── Pool-B / Proof-of-Improvement mint audit rows: minted_amount is the µLENS
--    actually credited via CreditHeldTx (the sweeper/revoker/resolver now read it
--    as int64). The USD / score basis columns (avoided_cogs_usd, similarity,
--    discrimination, skill_margin, latency_skill, …) stay DOUBLE (Tier-2/3).
ALTER TABLE pool_royalty_mints        ALTER COLUMN minted_amount TYPE BIGINT USING (round(minted_amount * 1000000))::bigint;
ALTER TABLE distill_royalty_mints     ALTER COLUMN minted_amount TYPE BIGINT USING (round(minted_amount * 1000000))::bigint;
ALTER TABLE eval_contribution_mints   ALTER COLUMN minted_amount TYPE BIGINT USING (round(minted_amount * 1000000))::bigint;
ALTER TABLE routing_prediction_mints  ALTER COLUMN minted_amount TYPE BIGINT USING (round(minted_amount * 1000000))::bigint;
ALTER TABLE node_latency_mints        ALTER COLUMN minted_amount TYPE BIGINT USING (round(minted_amount * 1000000))::bigint;
ALTER TABLE confidential_compute_mints ALTER COLUMN minted_amount TYPE BIGINT USING (round(minted_amount * 1000000))::bigint;

COMMIT;
