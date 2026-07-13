-- 0090_traffic_mint_holds.sql — Phase-1 Item 1: the holdback window for the
-- CreditOnce traffic mints (cache / compute / embedding).
--
-- WHY: those mints landed DIRECTLY in spendable balance (via CreditOnce → applyTx)
-- — no held state, so NO clawback surface. If gamed, there was nothing to reverse,
-- and the Phase-2 anti-gaming layer (which sits between holdback and settlement)
-- was structurally impossible. This routes them through the SAME proven held
-- machinery pool-royalty already uses (CreditHeldTx → finalize sweeper →
-- FinalizeHeldTx; RevokeHeldTx for reversal): every mint lands HELD and becomes
-- spendable only on finalize, after a configurable window (default 72h).
--
-- Columns mirror the generic finalize shape the sweeper expects
-- (request_id / workspace_id / minted_amount / status / finalize_after) PLUS
-- mint_type, so finalize writes the correct COUNTED supply ledger type per mint
-- (cache_mine, compute_mine, embedding_mine) rather than a single generic type.
-- PK (request_id, workspace_id, mint_type) is the exactly-once claim — a mint that
-- is held, finalized, then re-attempted conflicts here and does not double-mint.
CREATE TABLE IF NOT EXISTS traffic_mint_holds (
    request_id     TEXT        NOT NULL,
    workspace_id   TEXT        NOT NULL,          -- the beneficiary (owner earning the mint)
    mint_type      TEXT        NOT NULL,          -- the FINAL/counted ledger type written at finalize
    minted_amount  BIGINT      NOT NULL,          -- µLENS (SEC-2 integer smallest-unit)
    status         TEXT        NOT NULL DEFAULT 'held',  -- held | final | revoked
    finalize_after TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (request_id, workspace_id, mint_type)
);

-- Backs the sweeper's due-held scan (status='held' AND finalize_after < now()).
CREATE INDEX IF NOT EXISTS idx_traffic_mint_holds_finalize
    ON traffic_mint_holds (finalize_after) WHERE status = 'held';
