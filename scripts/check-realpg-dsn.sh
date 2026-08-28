#!/usr/bin/env bash
# check-realpg-dsn.sh — PREFLIGHT for the real-Postgres test suite.
#
# WHY THIS EXISTS. Every real-PG test in this repository is gated like this:
#
#     url := os.Getenv("LENS_TEST_DATABASE_URL")
#     if url == "" { t.Skip("… skipping real-PG …") }
#
# — 132 such skip sites across 151 files, with no shared helper. And `go test`
# EXITS 0 ON A SKIP. So if the Postgres service failed to come up, or the DSN
# changed, or the env line were dropped from the `go test` step, the ENTIRE
# real-PG suite would silently vanish and CI would stay green. That is a guard
# that cannot fail, one level up from the code.
#
# WHAT THIS CHECKS, AND WHAT IT DELIBERATELY DOES NOT.
# It proves the DSN the test step will use is SET and REACHABLE. It does not
# enumerate tests and makes no claim about how many real-PG tests exist or ran.
#
# That scope is deliberate and it is borrowed from a mistake a sibling repo
# already paid for. talyvor-docs shipped the obvious alternative — a CI step
# running `go test -v -run 'RealPG|SEC4|A3'` and failing on any skip — and then
# REMOVED it, because selecting tests BY NAME matched 36 of its 78 real-PG tests
# (46%) while a passing step implied protection over all of them. Their note:
# "A guard built to prevent green-by-absence was itself covering almost half of
# what it named, and nothing about it passing said so."
#
# Docs' real fix was to make its test helper FAIL rather than skip on a missing
# DSN, covering everything by construction. This repo has no such helper and 132
# inline skip sites; introducing one is a 151-file refactor and is filed as the
# complete fix. This preflight closes the failure mode that has an actual
# mechanism behind it — the DSN being absent or dead — with no coverage
# fraction to misreport.
#
# LOAD-BEARING: the DSN is defined ONCE at job level in ci.yaml and inherited by
# both this step and the `go test` step. If it were defined per-step, this could
# pass against one value while the tests skipped on another.
set -uo pipefail

if [ -z "${LENS_TEST_DATABASE_URL:-}" ]; then
  echo "::error::LENS_TEST_DATABASE_URL is unset. Every real-PG test would t.Skip and"
  echo "::error::\`go test\` would exit 0 — the suite would be green by absence."
  exit 1
fi

# ⚠ psql ABSENCE MUST BE ITS OWN ERROR, NOT A CONNECTION FAILURE. Measured: the
# first draft of this script had no such branch, and on a machine without psql it
# reported "the DSN is NOT reachable" — pointing at the database when the problem
# was the toolchain. A reader repairing that would have gone looking at Postgres.
# Worse, the obvious "fix" for a noisy missing-psql red is to tolerate it, and a
# tolerated missing tool is green-by-absence — precisely what this script exists
# to prevent.
if ! command -v psql >/dev/null 2>&1; then
  echo "::error::psql is not installed, so this preflight cannot verify the DSN."
  echo "::error::This is a TOOLCHAIN failure, not a database one. Do NOT make this"
  echo "::error::branch pass: a preflight that skips itself is the defect it guards against."
  exit 1
fi

# ⚠ KNOWN LIMIT, stated rather than discovered later: psql and pgx do not parse
# every DSN form identically. This proves the string is usable BY PSQL. A DSN
# that psql accepts and pgx rejects would pass here and still skip the tests.
#
# `psql "$DSN" -c 'select 1'` — the same connection string the tests are handed,
# so this cannot pass against a different database than they will use.
if ! out="$(psql "$LENS_TEST_DATABASE_URL" -tAc 'select 1' 2>&1)"; then
  echo "::error::LENS_TEST_DATABASE_URL is set but NOT reachable, so every real-PG test"
  echo "::error::would t.Skip and the suite would be green by absence. psql said:"
  echo "$out"
  exit 1
fi

if [ "$(echo "$out" | tr -d '[:space:]')" != "1" ]; then
  echo "::error::the DSN connected but 'select 1' returned '$out' — refusing to trust it"
  exit 1
fi

echo "real-PG preflight ok: LENS_TEST_DATABASE_URL is set and reachable"
echo "  (this proves the DSN is usable; it makes NO claim about how many real-PG tests ran)"
