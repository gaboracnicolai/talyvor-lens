-- B32.16 — the marketplace journal: every clearing and refund on double-entry postings.
--
-- A use can cost less than a cent (a 0.05 LXC use is $0.005), so the journal counts micro-dollars, as
-- market_earnings, market_refunds and market_payouts do: int64 µUSD, with the currency on every posting.
-- Debits are positive and credits negative. An entry whose postings do not sum to zero per currency is
-- refused when its transaction commits; a posting is never changed or deleted; and each account's balance
-- is kept in market_journal_balances by a trigger, in the transaction that writes the posting, as
-- agent_account_balances is (0185).
--
-- Accounts: stripe:clearing (what Stripe collected for the marketplace), revenue:market_fee (Talyvor's
-- take), seller:<workspace>:holdback and seller:<workspace>:available (what a seller is owed, inside and
-- past the 14-day holdback), rounding:stripe (Stripe charges whole cents), stripe:connect_fees (Stripe's
-- payout fees, at cost) and credits:issued (earnings taken as Talyvor credits).
--
-- Kinds: clear (a paid invoice clears a use), reversal (a refund or chargeback, the mirror of the use's
-- clear), and release, payout, credits and rounding, which B32.17 posts.
--
-- funding is the money's: live only for an earning a live-mode invoice paid to a real workspace (B22.1, B25.1).

CREATE TABLE IF NOT EXISTS market_journal_entries (
    id         TEXT PRIMARY KEY,  -- mje_<uuid>
    kind       TEXT NOT NULL CHECK (kind IN ('clear', 'reversal', 'release', 'payout', 'credits', 'rounding')),
    ref        TEXT NOT NULL,     -- what it explains: the use (clear, reversal, release), the payout, the invoice (rounding)
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (kind, ref)            -- a use clears once and is reversed once
);

CREATE TABLE IF NOT EXISTS market_journal_postings (
    entry_id          TEXT NOT NULL REFERENCES market_journal_entries (id),
    line              SMALLINT NOT NULL CHECK (line > 0),
    account           TEXT NOT NULL CHECK (account IN ('stripe:clearing', 'revenue:market_fee', 'rounding:stripe',
                                                       'stripe:connect_fees', 'credits:issued')
                                           OR account ~ '^seller:.+:(holdback|available)$'),
    amount_usd_micros BIGINT NOT NULL CHECK (amount_usd_micros <> 0),  -- debit positive, credit negative
    currency          CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
    funding           TEXT NOT NULL CHECK (funding IN ('test', 'live')),
    PRIMARY KEY (entry_id, line)
);
CREATE INDEX IF NOT EXISTS idx_market_journal_postings_account ON market_journal_postings (account);

-- An entry balances, per currency, or its transaction does not commit.
CREATE OR REPLACE FUNCTION market_journal_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    off RECORD;
BEGIN
    SELECT currency, sum(amount_usd_micros) AS total INTO off FROM market_journal_postings
     WHERE entry_id = NEW.entry_id GROUP BY currency HAVING sum(amount_usd_micros) <> 0 LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'market journal entry % does not balance: its % postings sum to % µUSD', NEW.entry_id, off.currency, off.total
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS market_journal_balanced ON market_journal_postings;
CREATE CONSTRAINT TRIGGER market_journal_balanced AFTER INSERT ON market_journal_postings
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION market_journal_balanced();

-- The journal is append-only, like every other money record.
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON market_journal_entries
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_journal_entries
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_mutation BEFORE UPDATE OR DELETE ON market_journal_postings
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_journal_postings
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- Each account's total, per currency and funding: the postings stay the record, this is their sum.
CREATE TABLE IF NOT EXISTS market_journal_balances (
    account            TEXT NOT NULL,
    currency           CHAR(3) NOT NULL,
    funding            TEXT NOT NULL,
    balance_usd_micros BIGINT NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account, currency, funding)
);

CREATE OR REPLACE FUNCTION market_journal_balances_post() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO market_journal_balances (account, currency, funding, balance_usd_micros)
    VALUES (NEW.account, NEW.currency, NEW.funding, NEW.amount_usd_micros)
    ON CONFLICT (account, currency, funding) DO UPDATE
        SET balance_usd_micros = market_journal_balances.balance_usd_micros + EXCLUDED.balance_usd_micros, updated_at = now();
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS market_journal_balances_post ON market_journal_postings;
CREATE TRIGGER market_journal_balances_post AFTER INSERT ON market_journal_postings
    FOR EACH ROW EXECUTE FUNCTION market_journal_balances_post();

-- A refund or chargeback of a cleared use posts the exact mirror of its clear entry, in the transaction that
-- writes the market_refunds row — whichever writes it: ReverseInvoice, refundUses, or a test-money crossing's
-- reversal (internal/economy). A use refunded before it cleared was never in the journal, and posts nothing.
CREATE OR REPLACE FUNCTION market_journal_reverse_use(p_use_id TEXT, p_at TIMESTAMPTZ) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    cleared  TEXT;
    reversal TEXT;
BEGIN
    SELECT id INTO cleared FROM market_journal_entries WHERE kind = 'clear' AND ref = p_use_id;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    reversal := 'mje_' || gen_random_uuid();
    INSERT INTO market_journal_entries (id, kind, ref, created_at) VALUES (reversal, 'reversal', p_use_id, p_at);
    INSERT INTO market_journal_postings (entry_id, line, account, amount_usd_micros, currency, funding)
    SELECT reversal, line, account, -amount_usd_micros, currency, funding
      FROM market_journal_postings WHERE entry_id = cleared ORDER BY line;
END $$;

CREATE OR REPLACE FUNCTION market_refunds_journal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM market_journal_reverse_use(NEW.use_id, NEW.refunded_at);
    RETURN NULL;
END $$;

-- Backfill: every earning already cleared, and every refund of one, so the journal holds the whole history.
-- No earning or refund lands between the triggers and the backfill, so each is posted exactly once.
LOCK TABLE market_earnings, market_refunds IN SHARE ROW EXCLUSIVE MODE;
DROP TRIGGER IF EXISTS market_refunds_journal ON market_refunds;
CREATE TRIGGER market_refunds_journal AFTER INSERT ON market_refunds
    FOR EACH ROW EXECUTE FUNCTION market_refunds_journal();

CREATE TEMP TABLE market_journal_backfill ON COMMIT DROP AS
SELECT 'mje_' || gen_random_uuid() AS id, e.use_id, e.seller_workspace_id, e.gross_usd_micros, e.share_usd_micros, e.cleared_at,
       CASE WHEN e.livemode AND NOT e.test THEN 'live' ELSE 'test' END AS funding
  FROM market_earnings e
 WHERE e.gross_usd_micros > 0
   AND NOT EXISTS (SELECT 1 FROM market_journal_entries j WHERE j.kind = 'clear' AND j.ref = e.use_id);

INSERT INTO market_journal_entries (id, kind, ref, created_at)
SELECT id, 'clear', use_id, cleared_at FROM market_journal_backfill;

-- A clear: +gross stripe:clearing, −fee revenue:market_fee, −share seller holdback. The fee is what Talyvor kept,
-- gross − share, for earnings split before B32.8 too (their fee_usd_micros reads 0).
INSERT INTO market_journal_postings (entry_id, line, account, amount_usd_micros, currency, funding)
SELECT b.id, p.line, p.account, p.amount, 'USD', b.funding
  FROM market_journal_backfill b
 CROSS JOIN LATERAL (VALUES (1::smallint, 'stripe:clearing', b.gross_usd_micros),
                            (2::smallint, 'revenue:market_fee', -(b.gross_usd_micros - b.share_usd_micros)),
                            (3::smallint, 'seller:' || b.seller_workspace_id || ':holdback', -b.share_usd_micros)) AS p (line, account, amount)
 WHERE p.amount <> 0;

SELECT market_journal_reverse_use(r.use_id, r.refunded_at)
  FROM market_refunds r JOIN market_journal_backfill b ON b.use_id = r.use_id
 ORDER BY r.refunded_at, r.use_id;
