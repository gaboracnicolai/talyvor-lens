#!/usr/bin/env bash
# Provision the scenario workspaces for the routing-pattern earning trial.
# Mints a tlv_ key per workspace (explicit distinct workspace_id — omitting it
# defaults to "default", which earn excludes) and opts in the ones that should
# earn. Opt-in uses the admin key (admin bypasses workspace isolation).
#
#   Run from the repo root:  LENS_API_KEY=... bash tools/trial/seed.sh
#
# Writes  tools/trial/keys.tsv  as  <workspace_id>\t<tlv_key>  for traffic.sh
# (gitignored).
set -euo pipefail
BASE=${BASE:-http://localhost:8080}
ADMIN=${LENS_API_KEY:?set LENS_API_KEY (must match the value in .env)}
OUT=${OUT:-tools/trial/keys.tsv}
: > "$OUT"

mintkey() { # <workspace_id> -> tlv_ key, appended to keys.tsv
  local ws="$1" key
  key=$(curl -fsS -X POST "$BASE/v1/api/keys" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "{\"workspace_id\":\"$ws\",\"name\":\"trial\"}" \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)["key"])')
  printf '%s\t%s\n' "$ws" "$key" >> "$OUT"
  echo "$key"
}

optin() { # <workspace_id>  (admin-driven)
  curl -fsS -X POST "$BASE/v1/workspaces/$1/pattern-mining/opt-in" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' >/dev/null
  echo "  opted-in: $1"
}

register() { # <workspace_id>  (needed before SetLoggingPolicy)
  # A minimal {id,name} body is sufficient since #128/#138 — the server defaults
  # allowed_models/allowed_providers to empty (allow-all) and active to true.
  curl -fsS -X POST "$BASE/v1/workspaces" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "{\"id\":\"$1\",\"name\":\"$1\"}" >/dev/null
  echo "  registered: $1"
}

setlogging() { # <workspace_id> <policy>
  curl -fsS -X PUT "$BASE/v1/workspaces/$1/logging" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "{\"logging_policy\":\"$2\"}" >/dev/null
  echo "  logging=$2: $1"
}

echo "== scenario (a) base =="
mintkey ws-base       >/dev/null; optin ws-base

echo "== scenario (b) corroboration (4 opted-in) =="
for i in 1 2 3 4; do mintkey "ws-corr-$i" >/dev/null; optin "ws-corr-$i"; done

echo "== scenario (c) claim =="
mintkey ws-claim      >/dev/null; optin ws-claim

echo "== scenario (d) cap =="
mintkey ws-cap        >/dev/null; optin ws-cap

echo "== scenario (e1) non-opted-in control (NO opt-in) =="
mintkey ws-noopt      >/dev/null

echo "== scenario (e2) LoggingNone (registered + none + opted-in) =="
mintkey ws-none       >/dev/null
register ws-none
setlogging ws-none none
optin ws-none

echo
echo "keys -> $OUT"
cat "$OUT"
