-- 0116_request_attribution_request_id.sql
--
-- request_attribution already stores the issue the user was working on (issue_id, from the
-- X-Talyvor-Issue header the Code extension sends). What it never stored was WHICH REQUEST that
-- attribution belongs to: the table has its own UUID id and no request_id, while the spend rows
-- live in token_events keyed by request_id. Two parallel records of the same request, no shared key.
--
-- The consequence reached the product: Track credits an issue by matching the spend record's
-- FEATURE against an issue identifier, and the extension sends the feature as an IDE affordance
-- ("code-chat"), so every request from the editor we ship attributed to nothing in the tracker we
-- ship — even though the issue was known and stored the whole time.
--
-- ⚠ FORWARD-ONLY, DELIBERATELY. Rows written before this migration keep request_id = '' and stay
-- unjoinable; their spend remains unattributed for ever. Back-filling is not possible — the link
-- was never recorded — and inventing one by timestamp proximity would guess money onto issues.
-- Additive and defaulted, so the old binary runs against the new schema unchanged.
ALTER TABLE request_attribution
    ADD COLUMN IF NOT EXISTS request_id TEXT NOT NULL DEFAULT '';

-- Partial: only rows that actually carry a request id are worth indexing, and it keeps the index
-- off the historical rows that can never match.
CREATE INDEX IF NOT EXISTS idx_request_attribution_request_id
    ON request_attribution(request_id)
    WHERE request_id != '';
