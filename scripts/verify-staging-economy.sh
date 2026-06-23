#!/usr/bin/env bash
# verify-staging-economy.sh — READ-ONLY check that the distill reuse-royalty TEST
# economy is live and correct. Asserts, against $LENS_TEST_DATABASE_URL:
#   (1) basis recorded         — distill_royalty_basis has the (owner, requester) row (PR2)
#   (2) held mint == s × basis — distill_royalty_mints credits the OWNER s × avoided_cogs_usd (PR3)
#   (3) finalize → supply      — a finalized mint wrote the counted 'pool_royalty' ledger row
#
# It NEVER writes (SELECT-only). TEST/STAGING ONLY — see docs/staging-economy-turnon.md.
#
# Usage:  LENS_TEST_DATABASE_URL=postgres://… ./scripts/verify-staging-economy.sh [owner] [requester]
#         (defaults: owner=wsA requester=wsB; share from LENS_POOL_ROYALTY_SHARE, default 0.5)
set -euo pipefail

OWNER="${1:-wsA}"
REQUESTER="${2:-wsB}"
SHARE="${LENS_POOL_ROYALTY_SHARE:-0.5}"
: "${LENS_TEST_DATABASE_URL:?set LENS_TEST_DATABASE_URL to the TEST DB DSN (never production)}"

# PSQL_BIN lets a containerized/custom psql be substituted (default: psql on PATH).
PSQL=("${PSQL_BIN:-psql}" "$LENS_TEST_DATABASE_URL" -tAX -v ON_ERROR_STOP=1)
q() { "${PSQL[@]}" -c "$1" | tr -d '[:space:]'; }

fail=0
echo "distill reuse-royalty staging check — owner=$OWNER requester=$REQUESTER share=$SHARE"
echo "----------------------------------------------------------------------"

# (1) basis recorded
basis_n=$(q "SELECT count(*) FROM distill_royalty_basis WHERE owner_workspace_id='$OWNER' AND requester_workspace_id='$REQUESTER';")
if [ "${basis_n:-0}" -ge 1 ]; then
  cogs=$(q "SELECT avoided_cogs_usd FROM distill_royalty_basis WHERE owner_workspace_id='$OWNER' AND requester_workspace_id='$REQUESTER' LIMIT 1;")
  echo "PASS  (1) basis recorded         — $basis_n row(s), avoided_cogs_usd=$cogs"
else
  echo "FAIL  (1) basis recorded         — no distill_royalty_basis row for ($OWNER,$REQUESTER): did B reuse A's OCR cross-tenant with BOTH opted in + LENS_DISTILL_POOLABLE_ENABLED=true?"
  fail=1
fi

# (2) held mint == s × basis, credited to the OWNER (contributor)
mint_n=$(q "SELECT count(*) FROM distill_royalty_mints WHERE contributor_workspace_id='$OWNER';")
if [ "${mint_n:-0}" -ge 1 ]; then
  bad=$(q "SELECT count(*) FROM distill_royalty_mints m
             JOIN distill_royalty_basis b
               ON b.owner_workspace_id=m.contributor_workspace_id
              AND b.requester_workspace_id=m.requester_workspace_id
              AND b.content_hash=m.content_hash
           WHERE m.contributor_workspace_id='$OWNER'
             AND abs(m.minted_amount - ($SHARE)::float8 * b.avoided_cogs_usd) > 1e-9;")
  amt=$(q "SELECT minted_amount FROM distill_royalty_mints WHERE contributor_workspace_id='$OWNER' LIMIT 1;")
  st=$(q "SELECT status FROM distill_royalty_mints WHERE contributor_workspace_id='$OWNER' LIMIT 1;")
  if [ "${bad:-1}" = "0" ]; then
    echo "PASS  (2) held mint == ${SHARE}×basis — $mint_n mint(s) to $OWNER, minted_amount=$amt (status=$st)"
  else
    echo "FAIL  (2) held mint == ${SHARE}×basis — $bad mint(s) where minted_amount != ${SHARE} × avoided_cogs_usd"
    fail=1
  fi
else
  echo "FAIL  (2) held mint == ${SHARE}×basis — no distill_royalty_mints row for $OWNER: mint flag on AND owner verified (earn_verified OR completed lxc_purchase>0)? (the sweeper ticks ~1/min)"
  fail=1
fi

# (3) finalize → supply (PENDING during the holdback is not a failure)
final_n=$(q "SELECT count(*) FROM distill_royalty_mints WHERE contributor_workspace_id='$OWNER' AND status='final';")
if [ "${final_n:-0}" -ge 1 ]; then
  supply_rows=$(q "SELECT count(*) FROM lens_token_ledger WHERE workspace_id='$OWNER' AND type='pool_royalty';")
  if [ "${supply_rows:-0}" -ge 1 ]; then
    sup=$(q "SELECT coalesce(sum(amount),0) FROM lens_token_ledger WHERE workspace_id='$OWNER' AND type='pool_royalty';")
    echo "PASS  (3) finalize → supply      — $final_n finalized; counted 'pool_royalty' ledger sum for $OWNER = $sup"
  else
    echo "FAIL  (3) finalize → supply      — $final_n mint(s) marked 'final' but NO counted 'pool_royalty' ledger row for $OWNER"
    fail=1
  fi
else
  echo "PEND  (3) finalize → supply      — no 'final' mint yet (held within the ${LENS_POOL_HOLDBACK_WINDOW:-72h} holdback); re-run after the window"
fi

echo "----------------------------------------------------------------------"
if [ "$fail" = "0" ]; then
  echo "RESULT: OK — the distill test economy is live and correct (finalize may be PEND during the holdback)."
  exit 0
fi
echo "RESULT: FAIL — see the FAIL line(s) above + docs/staging-economy-turnon.md."
exit 1
