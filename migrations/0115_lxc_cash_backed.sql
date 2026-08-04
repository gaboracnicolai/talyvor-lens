-- CASH-BACKED LXC — the portion of a workspace's balance that real money paid for.
--
-- ⚠ THE DEFECT. lxc_ledger rows are TYPED (purchase / admin_grant / convert_from_lens) but
-- lxc_balances.balance is one fungible BIGINT, and SettleLXCReservation reads only that. Measured
-- against real Postgres on all 114 prior migrations, driven through the production seam
-- (agentReserveBlocks → settlePooledServe → mintPooledRoyalty), all three funding sources minted
-- BYTE-IDENTICALLY — 376 µLENS each. The source was not under-weighted; it was INVISIBLE.
--
-- A royalty minted against comped or converted credit is LENS issued with no cash behind it.
--
-- cash_backed_ulxc is lot accounting collapsed to ONE SCALAR: no lot table, no straddling rows, no
-- new scan. It is updated inside the transaction that already holds the balance row FOR UPDATE.
--
-- ⚠ SPEND CONSUMES UNBACKED FIRST, so cash_backed only falls once unbacked is exhausted. Ordering
-- does not change the TOTAL cash-backed spend over a fully-drained balance, but it changes every
-- PREFIX: a workspace holding 30 cash + 70 grant that spends 50 mints on 0 under unbacked-first and
-- on 30 under cash-first. Unbacked-first mints strictly less at every point in time, and it keeps
-- cash_backed available for a later refund to decrement rather than clamping at zero.
--
-- ⚠ BACKFILL: min(balance, completed purchases). Verified by case rather than assumed:
--   · purchases only, spent or not  → exact (all spend must have consumed cash);
--   · grants/conversions only       → 0, exact;
--   · mixed, grant arrived BEFORE the spending → exact (unbacked absorbed the spend).
--   It OVER-CREDITS in exactly one shape: cash spent BEFORE unbacked credit arrived
--   (purchase → spend → grant), where it restores backing the spending already consumed.
--   That shape can occur in future histories, but the backfill runs ONCE and the column is
--   maintained incrementally afterwards, so only histories existing AT MIGRATION TIME matter.
--   Operators can check for the shape before applying:
--     SELECT b.workspace_id FROM lxc_balances b
--      WHERE EXISTS (SELECT 1 FROM lxc_ledger l WHERE l.workspace_id=b.workspace_id AND l.type='purchase')
--        AND EXISTS (SELECT 1 FROM lxc_ledger l WHERE l.workspace_id=b.workspace_id
--                      AND l.type IN ('admin_grant','convert_from_lens'));
--   No rows ⇒ the backfill is exact on this deployment.
--
-- Additive and idempotent: ADD COLUMN IF NOT EXISTS, NOT NULL DEFAULT 0.
ALTER TABLE lxc_balances
  ADD COLUMN IF NOT EXISTS cash_backed_ulxc BIGINT NOT NULL DEFAULT 0;

-- Backfill. LEAST(balance, purchases) can never exceed the balance, so the invariant
-- cash_backed_ulxc <= balance holds from the first moment the column exists.
-- Refunded purchases are excluded: a refunded purchase left no cash in the system.
UPDATE lxc_balances b
SET cash_backed_ulxc = GREATEST(0, LEAST(
      b.balance::BIGINT,
      COALESCE((SELECT SUM(p.lxc_amount)::BIGINT
                  FROM lxc_purchases p
                 WHERE p.workspace_id = b.workspace_id
                   AND p.status = 'completed'), 0)))
WHERE b.cash_backed_ulxc = 0;
