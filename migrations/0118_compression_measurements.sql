-- 0118_compression_measurements.sql — THE DURABLE RECORD OF WHAT THE PROMPT
-- REWRITER ACTUALLY DID, and of whether it reached the bill.
--
-- WHY A NEW TABLE AND NOT token_events.savings_pct. That column is TOMBSTONED
-- (migration 0114): it has never had a writer, every row carries the default, and
-- reading it produced a wrong customer-facing number three separate times.
-- internal/catalog/writerless_column_guard_test.go fails the build if a fourth
-- writerless column appears. Reviving it as the home for a real measurement would
-- put a live number in a column the schema comment tells every reader to ignore.
--
-- ⚠ WHAT IS RECORDED IS BYTES AND A STRING COMPARISON, NEVER A PERCENTAGE. The
-- rewriter can change the bytes sent upstream while its own SavingsPct reads
-- exactly 0.00% — len/4 integer division swallows a one-byte change
-- (compressor.TestSavings_ZeroDoesNotMeanUntouched pins the case: a blank line
-- removed inside a fenced Python block). Any audit written as "look where savings
-- > 0" is blind to exactly the class the 0117 gate exists for, so `modified` here
-- is `sent != original` compared as STRINGS at the moment of the rewrite.
--
--   original_bytes  len(the prompt the CALLER sent)
--   sent_bytes      len(the prompt the PROVIDER received) — rebuildBody's payload
--   modified        the two strings differ. NOT derived from a percentage.
--
-- ⚠ AND THE HALF THAT MAKES IT HONEST: bytes removed from the wire are NOT money.
-- The post-serve estimate is len(ORIGINAL prompt)/4 (proxy.go), so on a request
-- the provider returns no usage block for, a workspace that opted IN is billed for
-- bytes it did not send and the saving reaches the bill by exactly zero. That is
-- not a defect this migration repairs — whose length the estimate should follow is
-- an open decision — it is a fact the measurement must not hide. So each row also
-- carries what was actually billed and which path produced it:
--
--   billed_input_tokens  the input tokens the spend row was written with
--   cost_estimated       true  ⇒ len(ORIGINAL)/4; the saving moved no money
--                        false ⇒ the provider counted what it actually received
--
-- A reader that reports bytes removed without reporting how many of those rows
-- were billed on the original would be describing a saving nobody received. That
-- is what the summary's estimated_path_requests COUNT is for.
--
-- ⚠ billed_input_tokens AND model ARE PER-ROW FORENSIC DETAIL AND NOTHING SUMS
-- THEM — stated here because a column with a writer and no reader is a question a
-- reader of this file WILL ask, and "nobody got round to it" is the wrong answer.
-- SUM(billed_input_tokens) would blend two different bases in one figure: on
-- cost_estimated rows the count is len(ORIGINAL)/4 and on the rest it is the
-- provider's own report. A total whose meaning shifts with the mix of the two is
-- the shape of number this schema family has already produced three times
-- (migration 0114). The honest aggregate is the COUNT of each basis, which is
-- what the reader serves; the per-row token figure is here so an operator can
-- answer a specific request after the fact, not so a dashboard can total it.
--
-- ⚠ ROWS ARE WRITTEN WHETHER OR NOT ANYTHING WAS SAVED. The zero is the finding:
-- the rewriter modified 0 of 308 committed corpus prompts. A table that only holds
-- hits cannot tell "the rewriter ran 10,000 times and saved nothing" from "the
-- rewriter never ran", and those are opposite answers. requests IS the denominator.
--
-- WRITER: internal/proxy/compression_measure.go, POST-FLUSH and void. No workspace
-- has the gate open (0117 backfilled every existing row to 'disabled'), so this
-- table is EMPTY BY CONSTRUCTION today and its reader says so rather than
-- rendering a zero.
--
-- ⚠ THE POPULATION IS NARROWER THAN "GATED REQUESTS", so a query that treats
-- COUNT(*) as the number of times the rewriter ran will be wrong. The write sits
-- inside the serve path's spend-row branch, so a row exists for a request only if
-- the gate opened AND the upstream answered 200 AND no guardrail blocked the
-- output AND the workspace is not LoggingNone AND the observational writer bound
-- had capacity. In one line: GATED REQUESTS THAT PRODUCED A SPEND ROW, minus
-- sheds. That is deliberate — it is what makes billed_input_tokens below real on
-- every row — but a rewritten prompt CAN reach a provider without appearing here.
-- READER: internal/api/compression_savings.go → GET /v1/workspaces/{id}/compression/savings.
--
-- request_id is the gateway X-Talyvor-Request-ID (proxy.go assigns one when the
-- caller sends none). PK + ON CONFLICT DO NOTHING = one measurement per request,
-- so a retried write cannot double-count the denominator.
--
-- ⚠ COMMENT CORRECTED BY 0119 — THE BARE `request_id PRIMARY KEY` BELOW WAS WRONG
-- AND THE PARAGRAPH ABOVE IS WHY IT LOOKED RIGHT. "one measurement per request"
-- is true; what it omits is that request_id is a string the CALLER supplies, so
-- the key was global over a value no tenant owns and the first workspace to
-- present one silently swallowed every other workspace's measurement of its own
-- request (ON CONFLICT DO NOTHING → Record returns nil → that workspace's
-- denominator reads 0). 0049 had already written the rule down for
-- pattern_mine_credits. 0119 moves the key to (request_id, workspace_id); read
-- that file before this one. The DDL below is left as applied — it is the record
-- of what shipped, not a description of the current schema.
CREATE TABLE IF NOT EXISTS compression_measurements (
    request_id          TEXT        PRIMARY KEY,          -- gateway request id; one measurement per request
    workspace_id        TEXT        NOT NULL,             -- tenant scope; the reader filters on this
    model               TEXT        NOT NULL,             -- the model actually sent upstream
    original_bytes      INTEGER     NOT NULL,             -- len(caller's prompt)
    sent_bytes          INTEGER     NOT NULL,             -- len(prompt the provider received)
    modified            BOOLEAN     NOT NULL,             -- sent != original, compared as STRINGS
    billed_input_tokens INTEGER     NOT NULL,             -- input tokens the spend row carried
    cost_estimated      BOOLEAN     NOT NULL,             -- true ⇒ billed on len(ORIGINAL)/4
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The reader's only query shape: one workspace, one window. Without this the
-- summary degrades to a seq scan as the table grows.
CREATE INDEX IF NOT EXISTS idx_compression_measurements_ws_created
    ON compression_measurements (workspace_id, created_at DESC);
