-- B19.17 — a scheduled payment to a marketplace listing.
--
-- A schedule pays either another agent of the workspace (B19.8, to_agent_id) or a marketplace listing
-- (to_listing_id), never both. A tick that pays a listing is a billed use of it (market_uses, B20.2),
-- judged by the paying agent's rules and metered onto the company's monthly marketplace bill; its run row
-- names that use.
ALTER TABLE agent_payment_schedules ALTER COLUMN to_agent_id DROP NOT NULL;
ALTER TABLE agent_payment_schedules ADD COLUMN IF NOT EXISTS to_listing_id TEXT;
ALTER TABLE agent_payment_schedules ADD CONSTRAINT agent_payment_schedules_one_payee
    CHECK ((to_agent_id IS NULL) <> (to_listing_id IS NULL));
ALTER TABLE agent_schedule_runs ADD COLUMN IF NOT EXISTS use_id TEXT;
