-- B32.26 — lineage royalties: market_earnings becomes one row per payee per sale.
--
-- A sale of a remix pays its originals: the seller's pool (the price less Talyvor's fee) flows up the version's
-- family tree (market_lineage, 0206), each parent receiving its edge's share of what its child received. Each payee
-- of a sale is one row: the seller's ('sale'), each ancestor's ('lineage', source_ref the market_lineage edge that
-- paid it, depth 1 for a parent) and, for B32.34 and B30.82, each declared share's ('split'). gross_usd_micros and
-- fee_usd_micros are on the sale row only, so a use's rows together still read gross = fee + Σ share.
--
-- Adding id with a volatile default gives every existing row its own id as the table is rewritten: no UPDATE, so
-- the append-only rule (0149) is never switched off.
ALTER TABLE market_earnings ADD COLUMN IF NOT EXISTS id TEXT NOT NULL DEFAULT ('mer_' || gen_random_uuid());
ALTER TABLE market_earnings ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'sale';
ALTER TABLE market_earnings ADD COLUMN IF NOT EXISTS source_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE market_earnings ADD COLUMN IF NOT EXISTS depth INTEGER NOT NULL DEFAULT 0;

ALTER TABLE market_earnings DROP CONSTRAINT IF EXISTS market_earnings_kind_check;
ALTER TABLE market_earnings ADD CONSTRAINT market_earnings_kind_check CHECK (
    (kind = 'sale' AND source_ref = '' AND depth = 0)
    OR (kind = 'split' AND source_ref <> '' AND depth = 0)
    OR (kind = 'lineage' AND source_ref <> '' AND depth > 0 AND gross_usd_micros = 0 AND fee_usd_micros = 0));

ALTER TABLE market_earnings DROP CONSTRAINT IF EXISTS market_earnings_pkey;
ALTER TABLE market_earnings ADD CONSTRAINT market_earnings_pkey PRIMARY KEY (id);
ALTER TABLE market_earnings DROP CONSTRAINT IF EXISTS market_earnings_payee;
ALTER TABLE market_earnings ADD CONSTRAINT market_earnings_payee UNIQUE (use_id, seller_workspace_id, kind, source_ref);
