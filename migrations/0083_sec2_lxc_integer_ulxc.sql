-- 0083_sec2_lxc_integer_ulxc.sql
--
-- SEC-2, LXC side. LXC is a USD-pegged one-way compute credit; the code has
-- always rounded LXC to 6 decimals (roundTo(_,6)), so its operative smallest unit
-- is 1e-6 LXC — the micro-LXC (µLXC): 1 LXC = 1_000_000 µLXC (BIGINT). Same 1e6
-- scale as µLENS. Converting the LXC ledger + balances + purchase/agent-budget
-- columns to BIGINT µLXC makes LXC accounting exact and conserving, matching the
-- LENS side (0082).
--
-- Tier-3 USD stays integer cents where it already is (lxc_purchases.usd_cents is
-- already BIGINT; the LXC peg LXCUSDValue = $0.10 is a Tier-2 constant in code).
--
-- lxc_ledger is PARTITIONED (0034); altering the parent propagates to partitions.

BEGIN;

-- ── LXC ledger (partitioned) ────────────────────────────────────────────────
ALTER TABLE lxc_ledger
    ALTER COLUMN amount        TYPE BIGINT USING (round(amount * 1000000))::bigint,
    ALTER COLUMN balance_after TYPE BIGINT USING (round(balance_after * 1000000))::bigint;

-- ── LXC balances (0027) ─────────────────────────────────────────────────────
ALTER TABLE lxc_balances
    ALTER COLUMN balance         TYPE BIGINT USING (round(balance * 1000000))::bigint,
    ALTER COLUMN lifetime_minted TYPE BIGINT USING (round(lifetime_minted * 1000000))::bigint,
    ALTER COLUMN lifetime_spent  TYPE BIGINT USING (round(lifetime_spent * 1000000))::bigint;

-- ── fiat purchase credited LXC (0054); usd_cents already BIGINT ─────────────
ALTER TABLE lxc_purchases
    ALTER COLUMN lxc_amount TYPE BIGINT USING (round(lxc_amount * 1000000))::bigint;

-- ── agent LXC sub-budgets (0079). The default ceiling 50 LXC becomes 50e6 µLXC.
ALTER TABLE agent_lxc_subbudgets
    ALTER COLUMN ceiling_lxc TYPE BIGINT USING (round(ceiling_lxc * 1000000))::bigint,
    ALTER COLUMN spent_lxc   TYPE BIGINT USING (round(spent_lxc * 1000000))::bigint;
ALTER TABLE agent_lxc_subbudgets
    ALTER COLUMN ceiling_lxc SET DEFAULT 50000000; -- 50 LXC in µLXC

ALTER TABLE lxc_spend_claims
    ALTER COLUMN lxc_amount TYPE BIGINT USING (round(lxc_amount * 1000000))::bigint;

COMMIT;
