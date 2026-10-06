-- B32.21 — free trials in a sandbox, on test money.
--
-- A per_use offer's trial_uses (0201) gives each buyer that many uses of its listing charged 'trial' (0202): never
-- metered and never an earning. Each trial use the buyer's run answered is journalled as one test-funded entry of kind
-- 'trial' — the split the use would have made had it been billed: +price trial:waived, −Talyvor's take
-- trial:market_fee, −the seller's share trial:seller:<workspace>. None of these is a seller's holdback or available,
-- so a trial never reaches a payout.
ALTER TABLE market_journal_entries DROP CONSTRAINT IF EXISTS market_journal_entries_kind_check;
ALTER TABLE market_journal_entries ADD CONSTRAINT market_journal_entries_kind_check
    CHECK (kind IN ('clear', 'reversal', 'release', 'payout', 'credits', 'rounding', 'trial'));

ALTER TABLE market_journal_postings DROP CONSTRAINT IF EXISTS market_journal_postings_account_check;
ALTER TABLE market_journal_postings ADD CONSTRAINT market_journal_postings_account_check
    CHECK (account IN ('stripe:clearing', 'revenue:market_fee', 'rounding:stripe', 'stripe:connect_fees', 'credits:issued',
                       'trial:waived', 'trial:market_fee')
           OR account ~ '^seller:.+:(holdback|available)$'
           OR account ~ '^trial:seller:.+$');

-- How many trial uses of a listing a buyer has had: counted on every use, under the listing's lock.
CREATE INDEX IF NOT EXISTS idx_market_uses_trial ON market_uses (listing_id, buyer_workspace_id) WHERE charge = 'trial';
