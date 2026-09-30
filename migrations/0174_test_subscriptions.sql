-- B25.2 — test users subscribe with Stripe test cards.
--
-- A synthetic (test) workspace now tops up and subscribes through Stripe test mode, so its subscription and the
-- included usage each period grants are test money too. They carry the test mark 0173 gives every money table,
-- set the same way: by mark_test_money() from workspaces.synthetic, never by the writer. Until now a synthetic
-- workspace could not subscribe, so there are no rows to backfill.

ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS test BOOLEAN NOT NULL DEFAULT false;
CREATE OR REPLACE TRIGGER mark_test_money BEFORE INSERT ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION mark_test_money('workspace_id');

ALTER TABLE subscription_allowance ADD COLUMN IF NOT EXISTS test BOOLEAN NOT NULL DEFAULT false;
CREATE OR REPLACE TRIGGER mark_test_money BEFORE INSERT ON subscription_allowance
    FOR EACH ROW EXECUTE FUNCTION mark_test_money('workspace_id');
