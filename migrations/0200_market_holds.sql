-- B32.17 — the holdback is the escrow: releases, payouts and credits post to the journal, and it reconciles.
--
-- A cleared earning waits in seller:<workspace>:holdback until its payable_at, 14 days after its invoice was paid.
-- Then the release job (internal/market/escrow.go) moves it to seller:<workspace>:available with one release entry,
-- once per earning, unless an open hold names its use. market_earnings stays append-only, so a hold — a buyer's
-- dispute (B32.48), an IP claim — is its own row: opened while the earning is still inside its 14 days (or before it
-- clears), and released once, with the decision. A payout moves available to stripe:clearing (the net) and
-- stripe:connect_fees (Stripe's fees, at cost); earnings taken as credits move it to credits:issued.

CREATE TABLE IF NOT EXISTS market_holds (
    id          TEXT PRIMARY KEY,  -- mhd_<uuid>
    use_id      TEXT NOT NULL,
    reason      TEXT NOT NULL CHECK (reason IN ('dispute', 'ip_claim')),
    opened_by   TEXT NOT NULL CHECK (opened_by <> ''),
    opened_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    decision    TEXT NOT NULL DEFAULT '',
    CHECK ((released_at IS NULL) = (decision = ''))  -- released with a decision, and only then
);
CREATE INDEX IF NOT EXISTS idx_market_holds_open ON market_holds (use_id) WHERE released_at IS NULL;

-- A hold is released once, and is otherwise never changed or deleted: the escrow cannot be emptied by editing it.
CREATE OR REPLACE FUNCTION market_holds_release_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.released_at IS NOT NULL OR NEW.released_at IS NULL
       OR (NEW.id, NEW.use_id, NEW.reason, NEW.opened_by, NEW.opened_at) IS DISTINCT FROM (OLD.id, OLD.use_id, OLD.reason, OLD.opened_by, OLD.opened_at) THEN
        RAISE EXCEPTION 'market hold %: a hold is only ever released, once', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE OR REPLACE TRIGGER market_holds_release_only BEFORE UPDATE ON market_holds
    FOR EACH ROW EXECUTE FUNCTION market_holds_release_only();
CREATE OR REPLACE TRIGGER audit_no_delete BEFORE DELETE ON market_holds
    FOR EACH ROW EXECUTE FUNCTION audit_block_mutation();
CREATE OR REPLACE TRIGGER audit_no_truncate BEFORE TRUNCATE ON market_holds
    FOR EACH STATEMENT EXECUTE FUNCTION audit_block_mutation();

-- A refund or chargeback still posts the mirror of the use's clear entry, but the seller's share comes back out of
-- the account it is in now: available once the earning was released — below zero if it was paid out too, which is
-- what payout.go calls owed. The use's lock (taken by the release job too) orders a refund and a release of the
-- same use, so each sees the other.
CREATE OR REPLACE FUNCTION market_journal_reverse_use(p_use_id TEXT, p_at TIMESTAMPTZ) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    cleared  TEXT;
    released BOOLEAN;
    reversal TEXT;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('market_use:' || p_use_id, 0));
    SELECT id INTO cleared FROM market_journal_entries WHERE kind = 'clear' AND ref = p_use_id;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    released := EXISTS (SELECT 1 FROM market_journal_entries WHERE kind = 'release' AND ref = p_use_id);
    reversal := 'mje_' || gen_random_uuid();
    INSERT INTO market_journal_entries (id, kind, ref, created_at) VALUES (reversal, 'reversal', p_use_id, p_at);
    INSERT INTO market_journal_postings (entry_id, line, account, amount_usd_micros, currency, funding)
    SELECT reversal, line,
           CASE WHEN released AND account ~ '^seller:.+:holdback$' THEN regexp_replace(account, ':holdback$', ':available') ELSE account END,
           -amount_usd_micros, currency, funding
      FROM market_journal_postings WHERE entry_id = cleared ORDER BY line;
END $$;

-- Backfill: every earning already past its holdback and never refunded is released (at its payable_at), and every
-- payout already made is journalled (at its created_at), so the journal starts out equal to what payout.go reads.
LOCK TABLE market_earnings, market_refunds, market_payouts IN SHARE ROW EXCLUSIVE MODE;

CREATE TEMP TABLE market_journal_release_backfill ON COMMIT DROP AS
SELECT 'mje_' || gen_random_uuid() AS id, e.use_id, e.seller_workspace_id, e.share_usd_micros, e.payable_at,
       CASE WHEN e.livemode AND NOT e.test THEN 'live' ELSE 'test' END AS funding
  FROM market_earnings e
 WHERE e.payable_at <= now() AND e.share_usd_micros > 0
   AND NOT EXISTS (SELECT 1 FROM market_refunds r WHERE r.use_id = e.use_id)
   AND NOT EXISTS (SELECT 1 FROM market_journal_entries j WHERE j.kind = 'release' AND j.ref = e.use_id);

INSERT INTO market_journal_entries (id, kind, ref, created_at)
SELECT id, 'release', use_id, payable_at FROM market_journal_release_backfill;

INSERT INTO market_journal_postings (entry_id, line, account, amount_usd_micros, currency, funding)
SELECT b.id, p.line, p.account, p.amount, 'USD', b.funding
  FROM market_journal_release_backfill b
 CROSS JOIN LATERAL (VALUES (1::smallint, 'seller:' || b.seller_workspace_id || ':holdback', b.share_usd_micros),
                            (2::smallint, 'seller:' || b.seller_workspace_id || ':available', -b.share_usd_micros)) AS p (line, account, amount);

-- A credits payout's test-funded part is what its lxc_ledger row recorded (B22.1); without one, it was test money.
-- (credits_ulxc is never 0 on a credits payout: market_payouts' own check.)
CREATE TEMP TABLE market_journal_payout_backfill ON COMMIT DROP AS
SELECT 'mje_' || gen_random_uuid() AS id, p.id AS payout_id, p.workspace_id, p.method, p.gross_usd_micros, p.net_usd_micros,
       p.account_fee_usd_micros + p.payout_fee_usd_micros AS fees_usd_micros, p.created_at,
       CASE WHEN p.livemode AND NOT p.test THEN 'live' ELSE 'test' END AS funding,
       CASE WHEN p.method <> 'credits' THEN 0
            WHEN p.test THEN p.gross_usd_micros  -- a test workspace's money is test money (B25.1)
            ELSE LEAST(p.gross_usd_micros, COALESCE(l.test_ulxc, p.credits_ulxc) * p.gross_usd_micros / p.credits_ulxc)
            END AS credits_test_usd_micros
  FROM market_payouts p
  LEFT JOIN (SELECT DISTINCT ON (metadata->>'market_payout_id') metadata->>'market_payout_id' AS payout_id,
                    (metadata->>'test_funded_ulxc')::bigint AS test_ulxc
               FROM lxc_ledger WHERE metadata ? 'market_payout_id' AND metadata ? 'test_funded_ulxc'
              ORDER BY metadata->>'market_payout_id', created_at) l ON l.payout_id = p.id
 WHERE NOT EXISTS (SELECT 1 FROM market_journal_entries j WHERE j.kind IN ('payout', 'credits') AND j.ref = p.id);

INSERT INTO market_journal_entries (id, kind, ref, created_at)
SELECT id, CASE WHEN method = 'stripe' THEN 'payout' ELSE 'credits' END, payout_id, created_at FROM market_journal_payout_backfill;

INSERT INTO market_journal_postings (entry_id, line, account, amount_usd_micros, currency, funding)
SELECT b.id, row_number() OVER (PARTITION BY b.id ORDER BY p.ord)::smallint, p.account, p.amount, 'USD', p.funding
  FROM market_journal_payout_backfill b
 CROSS JOIN LATERAL (VALUES
        (1, 'seller:' || b.workspace_id || ':available', b.gross_usd_micros, b.funding, b.method = 'stripe'),
        (2, 'stripe:clearing', -b.net_usd_micros, b.funding, b.method = 'stripe'),
        (3, 'stripe:connect_fees', -b.fees_usd_micros, b.funding, b.method = 'stripe'),
        (4, 'seller:' || b.workspace_id || ':available', b.credits_test_usd_micros, 'test', b.method = 'credits'),
        (5, 'credits:issued', -b.credits_test_usd_micros, 'test', b.method = 'credits'),
        (6, 'seller:' || b.workspace_id || ':available', b.gross_usd_micros - b.credits_test_usd_micros, 'live', b.method = 'credits'),
        (7, 'credits:issued', -(b.gross_usd_micros - b.credits_test_usd_micros), 'live', b.method = 'credits')
     ) AS p (ord, account, amount, funding, applies)
 WHERE p.applies AND p.amount <> 0;
