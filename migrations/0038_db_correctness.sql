-- 0038_db_correctness.sql
--
-- Two schema correctness fixes identified in a database audit:
--
-- 1. Missing FK index on marketplace_trades(listing_id)
--    Postgres does not auto-create indexes on foreign key columns.
--    Without this, any DELETE on marketplace_listings or a JOIN from
--    trades back to listings scans the entire trades table.
--
-- 2. session_turns FK missing ON DELETE action
--    The original REFERENCES sessions(id) defaulted to NO ACTION,
--    making it impossible to delete a session that has turns without
--    first manually purging the turns rows. ON DELETE CASCADE matches
--    the natural lifecycle: a deleted session's turns have no meaning.
--    ALTER TABLE … DROP CONSTRAINT + ADD CONSTRAINT is the safe way
--    to change FK behaviour on an existing table.
--    The constraint name was auto-assigned by Postgres as
--    session_turns_session_id_fkey (the default naming convention).

CREATE INDEX IF NOT EXISTS idx_marketplace_trades_listing
    ON marketplace_trades (listing_id);

ALTER TABLE session_turns
    DROP CONSTRAINT IF EXISTS session_turns_session_id_fkey;

ALTER TABLE session_turns
    ADD CONSTRAINT session_turns_session_id_fkey
    FOREIGN KEY (session_id) REFERENCES sessions(id)
    ON DELETE CASCADE;
