-- 0105_distill_royalty_basis_settled_charge.sql — the distill funding invariant.
--
-- THE HOLE (mirror of the pool-royalty hole #351, in the async shape): the DistillMinter sweeper mints a
-- reuse royalty off distill_royalty_basis.avoided_cogs_usd — what the reuser AVOIDED — regardless of what
-- the reuser was actually CHARGED. So a reuse funded by nothing (a plain key, an unmetered lane, a failed
-- settle) minted a real royalty. The pool fix tied the mint to the settle IN-REQUEST; the distill mint is
-- ASYNC (a sweeper runs later off the stored basis), so the charge must be RECORDED at serve time and READ
-- at mint time.
--
-- THE FIX: the basis gains settled_charge_usd — the USD the reuser was ACTUALLY charged for that reuse
-- (the settled reservation, from resolveCacheReservation which returns it since #351), written at serve
-- time. The sweeper mints s × settled_charge (NOT avoided_cogs) and skips any row whose charge is absent
-- or ≤ 0. So a royalty is funded by a real payment, and an unfunded reuse mints nothing.
--
-- NULLABLE, default NULL, ADD-only — 0055-safe (no UPDATE/DELETE on any ledger). Existing basis rows (and
-- any serve that has not yet been wired to record the charge) carry NULL and therefore MINT NOTHING — the
-- deliberate FAIL-CLOSED default: we never mint a royalty we cannot prove the consumer funded. Historical
-- rows from before this migration are unfunded by construction and correctly mint zero.

ALTER TABLE distill_royalty_basis
    ADD COLUMN IF NOT EXISTS settled_charge_usd DOUBLE PRECISION;
