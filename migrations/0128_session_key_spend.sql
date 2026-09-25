-- 0128_session_key_spend.sql — B9.8: what one browser-chat session has been charged.
--
-- A chat request is billed like any other (allowance first, then prepaid LXC). This is the
-- per-session running total, so a single chat session has a ceiling it cannot pass: the proxy
-- refuses a request, before the provider is called, when this total plus the request's
-- conservative estimate would exceed the bound.
--
-- Additive, own file, no row rewritten. A session minted before this column existed starts at 0.

ALTER TABLE session_keys
    ADD COLUMN IF NOT EXISTS spent_ulxc BIGINT NOT NULL DEFAULT 0
        CHECK (spent_ulxc >= 0);
