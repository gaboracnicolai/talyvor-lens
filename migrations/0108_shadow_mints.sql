-- 0108_shadow_mints.sql
-- Shadow mode for the six UNPROVEN mints: record what each WOULD have paid, credit nothing.
--
-- WHY A SEPARATE TABLE, AND NOT A COLUMN ON lens_token_ledger. This is the whole safety argument,
-- so it lives here rather than only in a commit message.
--
-- The inflation risk is not a bad mint. It is a shadow row counted by something downstream.
-- Twelve call sites currently aggregate lens_token_ledger — GetTotalSupply, GetCirculatingSupply,
-- the PR2 rate cap's SUM, the mining-stats readers in cache/compute/embedding/annotation mining,
-- the oracle, the haircut observer. If a shadow row lived in that table behind a `shadow = true`
-- column, every one of those twelve would need `AND NOT shadow`, forever, including the
-- thirteenth that someone writes next year. Miss one and shadow value enters a supply figure —
-- which is inflation with no bad mint anywhere in sight.
--
-- A shadow row in a DIFFERENT TABLE cannot be counted by a query that names lens_token_ledger.
-- The guarantee needs no discipline from future readers, and it converts an N-reader obligation
-- into a ONE-WRITER obligation: the only thing to guard is that nothing writes a shadow mint into
-- the ledger. That is a single site, and a source-level test pins it
-- (TestShadowMint_NeverTouchesTheTokenLedger).
--
-- STRUCTURALLY INCAPABLE OF BECOMING REAL. Three independent reasons, none of which is a flag:
--
--   1. NO CREDIT PATH READS THIS TABLE. Balances move through exactly two kernels — applyTx
--      (spendable) and heldInner (held) — and both write lens_token_ledger. Neither reads
--      lens_shadow_mints, and there is no settle/finalize path from here to there. Making a
--      shadow row real would mean WRITING NEW CODE that inserts into the ledger from this
--      table's contents: a visible, reviewable change, not a config flip.
--   2. THE AMOUNT IS NOT IN LEDGER UNITS. lens_token_ledger.amount is µLENS (BIGINT-conserved
--      elsewhere); would_mint_micro_lens here is deliberately named for what it is — a
--      COMPUTED HYPOTHETICAL. A copy-paste into a credit call is not a silent type match.
--   3. NO BALANCE COLUMN. There is no balance_after, so a row here cannot be part of a
--      running-balance chain. The ledger's invariant (balance_after = previous + amount) has no
--      counterpart to corrupt.
--
-- WHAT IT IS FOR. Measuring six mints nobody has validated, during a trial in which testers are
-- told plainly they are not being paid for these. Query it directly:
--
--   -- what each shadow mint would have paid, per mint kind
--   SELECT mint_type, count(*) AS moments,
--          sum(would_mint_micro_lens) AS would_mint_micro_lens
--     FROM lens_shadow_mints GROUP BY mint_type ORDER BY 3 DESC;
--
--   -- per workspace, for one kind
--   SELECT workspace_id, count(*), sum(would_mint_micro_lens)
--     FROM lens_shadow_mints WHERE mint_type = 'receipt_mine_provisional'
--    GROUP BY workspace_id ORDER BY 3 DESC;

CREATE TABLE IF NOT EXISTS lens_shadow_mints (
    id           BIGSERIAL PRIMARY KEY,
    -- The workspace that WOULD have earned. Not a foreign key into balances: nothing here
    -- participates in a balance.
    workspace_id TEXT   NOT NULL,
    -- The ledger txType the real mint would have used (e.g. receipt_mine_provisional,
    -- eval_contribution_held). Recorded so a reader can line a shadow row up against the mint
    -- moment it stands in for.
    mint_type    TEXT   NOT NULL,
    -- The computed hypothetical, in µLENS. DELIBERATELY not called `amount`: it is not an amount
    -- of anything anyone holds.
    would_mint_micro_lens BIGINT NOT NULL,
    -- Free-form provenance from the mint site (request id, basis, rate) so a number can be
    -- traced back to the work that produced it. Content-free by convention, like the ledger's.
    metadata     JSONB  NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The two reads this table exists to serve: by kind, and by workspace within a kind.
CREATE INDEX IF NOT EXISTS idx_shadow_mints_type_created
    ON lens_shadow_mints (mint_type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_shadow_mints_ws_type
    ON lens_shadow_mints (workspace_id, mint_type);

COMMENT ON TABLE lens_shadow_mints IS
    'What an unproven mint WOULD have paid. Observation only: no credit path reads this table, '
    'no row here is part of any balance, ledger sum, or supply figure. Never join this into a '
    'balance or supply query.';
COMMENT ON COLUMN lens_shadow_mints.would_mint_micro_lens IS
    'A computed hypothetical in µLENS. NOT a holding, NOT spendable, NOT part of supply.';
