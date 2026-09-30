-- B25.1 — the wall between test money and real money.
--
-- A synthetic (test) workspace's money moves only between test workspaces; the refusal of anything across is
-- workspace.CheckMoneyWall, called by every money path. This migration is the other half: EVERY ROW TEST MONEY
-- WRITES CARRIES THE TEST MARK, so every real figure can leave it out.
--
-- The mark is `test`, and it is the database's, never the writer's: a BEFORE INSERT trigger sets it from
-- workspaces.synthetic of the row's workspace column(s) — true when any of them is a test workspace. So a code
-- path that forgets the mark cannot write a real-looking row, and none can mark a real row as test. The flag is
-- never cleared and a real workspace can never become synthetic (CreateSynthetic refuses an existing id), so the
-- mark of a row never changes after it is written.
--
-- Backfill: the rows test workspaces already wrote are marked here. Several of these tables are append-only
-- (0055, 0140, …): their audit_no_mutation trigger is switched off for the one UPDATE that sets the new column and
-- switched back on in the same transaction, so the append-only rule holds for every amount before and after.

CREATE OR REPLACE FUNCTION mark_test_money() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    r   jsonb := to_jsonb(NEW);
    col text;
BEGIN
    NEW.test := false;
    FOREACH col IN ARRAY TG_ARGV LOOP
        IF EXISTS (SELECT 1 FROM workspaces WHERE id = r ->> col AND synthetic) THEN
            NEW.test := true;
            EXIT;
        END IF;
    END LOOP;
    RETURN NEW;
END;
$$;

DO $$
DECLARE
    -- table, then the workspace column(s) whose flag marks its rows.
    marked text[][] := ARRAY[
        ['lxc_ledger',                'workspace_id',             ''],
        ['lxc_purchases',             'workspace_id',             ''],
        ['lens_token_ledger',         'workspace_id',             ''],
        ['pool_royalty_mints',        'requester_workspace_id',   'contributor_workspace_id'],
        ['distill_royalty_mints',     'requester_workspace_id',   'contributor_workspace_id'],
        ['agent_postings',            'workspace_id',             ''],
        ['agent_transfers',           'from_workspace_id',        'to_workspace_id'],
        ['agent_money_requests',      'from_workspace_id',        'to_workspace_id'],
        ['agent_loans',               'lender_workspace_id',      'borrower_workspace_id'],
        ['agent_escrows',             'payer_workspace_id',       'payee_workspace_id'],
        ['agent_pots',                'workspace_id',             ''],
        ['agent_payment_schedules',   'workspace_id',             ''],
        ['agent_topups',              'workspace_id',             ''],
        ['agent_cards',               'workspace_id',             ''],
        ['agent_card_authorizations', 'workspace_id',             ''],
        ['agent_card_settlements',    'workspace_id',             ''],
        ['agent_cash_outs',           'workspace_id',             ''],
        ['agent_debit_settlements',   'workspace_id',             ''],
        ['market_uses',               'buyer_workspace_id',       'seller_workspace_id'],
        ['market_earnings',           'seller_workspace_id',      ''],
        ['market_refunds',            'buyer_workspace_id',       'seller_workspace_id'],
        ['market_payouts',            'workspace_id',             '']
    ];
    i        int;
    tbl      text;
    cols     text[];
    pred     text;
    guarded  boolean;
BEGIN
    FOR i IN 1 .. array_length(marked, 1) LOOP
        tbl  := marked[i][1];
        cols := array_remove(ARRAY[marked[i][2], marked[i][3]], '');
        EXECUTE format('ALTER TABLE %I ADD COLUMN IF NOT EXISTS test BOOLEAN NOT NULL DEFAULT false', tbl);

        SELECT string_agg(format('%I IN (SELECT id FROM workspaces WHERE synthetic)', c), ' OR ')
          INTO pred FROM unnest(cols) AS c;
        guarded := EXISTS (SELECT 1 FROM pg_trigger
                            WHERE tgrelid = tbl::regclass AND tgname = 'audit_no_mutation' AND NOT tgisinternal);
        IF guarded THEN
            EXECUTE format('ALTER TABLE %I DISABLE TRIGGER audit_no_mutation', tbl);
        END IF;
        EXECUTE format('UPDATE %I SET test = true WHERE NOT test AND (%s)', tbl, pred);
        IF guarded THEN
            EXECUTE format('ALTER TABLE %I ENABLE TRIGGER audit_no_mutation', tbl);
        END IF;

        EXECUTE format('CREATE OR REPLACE TRIGGER mark_test_money BEFORE INSERT ON %I '
                       'FOR EACH ROW EXECUTE FUNCTION mark_test_money(%s)',
                       tbl, (SELECT string_agg(quote_literal(c), ', ') FROM unnest(cols) AS c));
    END LOOP;
END;
$$;
