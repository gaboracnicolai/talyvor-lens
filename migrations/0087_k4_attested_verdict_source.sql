-- 0087_k4_attested_verdict_source.sql
-- ATTESTED SEAM (step 1 of 3). Constrain k4_mechanical_verdicts.verdict_source now that a second, ATTESTED
-- source exists in the code: 'talyvor_verified' (Talyvor's own sandboxed re-run). NO PRODUCER writes it yet
-- (the sandboxed compile executor is step 2); this migration only makes the storage layer safe for it.
--
-- Two CHECKs, defense in depth for the money path (a false slash BURNS an honest workspace's collateral):
--
-- (1) known-source: verdict_source ∈ ('self_reported','talyvor_verified') — an unknown/forged source
--     ('hacker_says_so', 'attested_by_me', …) can NEVER be inserted.
--
-- (2) attested-compile-only: a 'talyvor_verified' row may ONLY carry a COMPILE verdict (compiled /
--     compile_failed). A 'talyvor_verified' TEST verdict is UNREPRESENTABLE — most importantly
--     (talyvor_verified, tests_failed), the row that would authorize a FALSE SLASH. Rationale (mirrors
--     outputverify.IsSlashUsable): `go test` executes arbitrary code (flaky tests, t.Parallel races,
--     time.Now, RNG, network, CGO) so a test verdict is NOT reproducible across environments; attesting it
--     would burn honest workspaces for environmental differences. Only `go build` (pinned toolchain, verified
--     deps, no network, no CGO) is deterministic AND runs no target code. So Talyvor attests ONLY the compile
--     verdict, and the schema makes any other attested row impossible. Both this CHECK and IsSlashUsable must
--     fail before a false slash can occur.
--
-- Existing rows are all 'self_reported' (the only source written to date) and satisfy both CHECKs, so the
-- ALTERs are safe to apply in place.

ALTER TABLE k4_mechanical_verdicts
    ADD CONSTRAINT k4_mech_verdict_source_known
    CHECK (verdict_source IN ('self_reported', 'talyvor_verified'));

ALTER TABLE k4_mechanical_verdicts
    ADD CONSTRAINT k4_mech_attested_is_compile_only
    CHECK (verdict_source <> 'talyvor_verified' OR verdict IN ('compiled', 'compile_failed'));
