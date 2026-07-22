#!/usr/bin/env bash
# onboard-trial-user.sh — ONE invocation onboards ONE trial user:
#   create the workspace → mint a proxy-scoped key → fund it with a comped LXC grant.
#
# This is the scripted form of docs/remote-host.md §5 (read that first — it has the
# LENS_API_KEY-over-loopback ritual around these calls and the unset-again step).
#
# WHY THE FUNDING STEP EXISTS: a fresh workspace has 0 LXC, and on the workspace-key
# lane its very first request is blocked PRE-SERVE by the AGENT ALLOCATOR — default-on
# (LENS_LXC_AGENT_ALLOCATION_ENABLED, internal/config/config.go) and fail-closed
# (internal/proxy/agent_allocator.go): insufficient balance ⇒
#   402 {"error":"agent LXC sub-budget exceeded or insufficient balance"}
# It is NOT the LXC gate (LENS_LXC_GATING_ENABLED + LENS_LXC_SHADOW_SPEND_ENABLED —
# a separate, default-off mechanism). Funding is the third of the three admin acts.
#
# Usage:
#   LENS_API_KEY=… ./scripts/onboard-trial-user.sh <workspace-id> [display-name] [grant-ulxc]
#
#   workspace-id   non-empty, [A-Za-z0-9._-] only, ≤64 chars (also names the key)
#   display-name   defaults to the workspace id (the server requires a non-empty name)
#   grant-ulxc     positive integer µLXC; default 50000000 = 50 LXC = $5 at the fixed
#                  $0.10/LXC peg. DELIBERATELY equal to the per-scoped-key sub-budget
#                  ceiling (DefaultAgentCeilingLXC = 50_000_000 µLXC,
#                  internal/economy/agent_subbudget.go): a one-key workspace can spend
#                  at most 50 LXC without a store-level ceiling raise (there is no HTTP
#                  route for that), so granting more strands the excess behind the
#                  ceiling — granting exactly 50 makes grant == ceiling, all spendable.
#                  Comped LXC cannot compound (an admin_grant never satisfies the
#                  verified-to-earn floor, internal/earnverify/verify.go), so total
#                  exposure is exactly the grant.
#
# Environment:
#   LENS_API_KEY     admin god key (required). Typical session (docs/remote-host.md §4):
#                      ssh -N -L 8080:127.0.0.1:8080 user@lens.example.com &
#                      export LENS_API_KEY=…   # the value set in the VM's .env
#                    (running ON the VM instead: export LENS_API_KEY=$(grep '^LENS_API_KEY=' .env | cut -d= -f2))
#   LENS_URL         base URL; default http://127.0.0.1:8080 (the tunnel / loopback)
#   LENS_PUBLIC_URL  printed in the hand-off snippet; default https://lens.example.com
#
# Server prerequisite: LENS_ADMIN_LXC_GRANT_ENABLED=true in the server .env (else the
# grant route is UNREGISTERED → 404; see .env.production.example). Set it in the same
# restart that sets LENS_API_KEY.
#
# NOT idempotent, on purpose — it REFUSES to run against an existing workspace id:
# POST /v1/workspaces is a blind upsert (re-posting resets the workspace's config to
# this script's minimal body), and the grant would re-grant (lxc_ledger is append-only).
# One id, one run. For anything unusual, run the individual curls from §5.

set -euo pipefail

DEFAULT_GRANT_ULXC=50000000   # 50 LXC = $5 == DefaultAgentCeilingLXC (see header)
CEILING_ULXC=50000000

LENS_URL="${LENS_URL:-http://127.0.0.1:8080}"
LENS_PUBLIC_URL="${LENS_PUBLIC_URL:-https://lens.example.com}"

die()   { printf 'onboard-trial-user: ERROR: %s\n' "$*" >&2; exit 1; }
usage() {
  printf 'usage: LENS_API_KEY=… %s <workspace-id> [display-name] [grant-ulxc]\n' "$0" >&2
  # shellcheck disable=SC2016 # the $5 is a literal dollar amount, not an expansion
  printf '       (defaults: display-name = workspace-id, grant-ulxc = %s = 50 LXC = $5)\n' "$DEFAULT_GRANT_ULXC" >&2
  printf '       full context: docs/remote-host.md §5 and the header of this file\n' >&2
  exit 1
}

# ── the god key, checked FIRST (before args, before any network I/O) ─────────
# A missing key must not surface later as a 401 from the API — that is a confusing
# way to learn you skipped the setup step, and the operator has already typed a
# workspace name by then. Empty is as bad as unset. Refuse to start at all.
[ -n "${LENS_API_KEY:-}" ] || {
  printf 'onboard-trial-user: ERROR: LENS_API_KEY is unset or empty — refusing to start.\n' >&2
  printf '  This script is three admin calls; without the god key each would just 401.\n' >&2
  printf '  Do the tunnel ritual first (docs/remote-host.md §5 step 0):\n' >&2
  printf '    on the VM:  set LENS_API_KEY (and LENS_ADMIN_LXC_GRANT_ENABLED=true) in .env,\n' >&2
  printf '                then:  docker compose up -d lens\n' >&2
  printf '    locally:    ssh -N -L 8080:127.0.0.1:8080 user@lens.example.com &\n' >&2
  printf '                export LENS_API_KEY=…   # the same value you set on the VM\n' >&2
  exit 1
}

[ $# -ge 1 ] && [ $# -le 3 ] || usage
WS_ID="$1"
WS_NAME="${2:-$WS_ID}"
GRANT_ULXC="${3:-$DEFAULT_GRANT_ULXC}"

command -v curl >/dev/null 2>&1 || die "curl not found on PATH"

# ── input validation (these values are spliced into JSON bodies — be strict) ──
case "$WS_ID" in
  ('') die "workspace-id must be non-empty" ;;
  (*[!A-Za-z0-9._-]*) die "workspace-id may contain only [A-Za-z0-9._-], got: $WS_ID" ;;
esac
[ "${#WS_ID}" -le 64 ] || die "workspace-id longer than 64 chars"

case "$WS_NAME" in
  (*[[:cntrl:]]*) die "display-name contains control characters" ;;
esac
# JSON-escape the only two characters that need it after the control-char refusal.
WS_NAME_JSON=$(printf '%s' "$WS_NAME" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')

case "$GRANT_ULXC" in
  (''|*[!0-9]*) die "grant-ulxc must be a positive integer in µLXC, got: $GRANT_ULXC" ;;
  (0*) die "grant-ulxc must be positive with no leading zero, got: $GRANT_ULXC" ;;
esac
if [ "$GRANT_ULXC" -gt "$CEILING_ULXC" ]; then
  printf 'onboard-trial-user: WARNING: %s µLXC exceeds the default per-key sub-budget ceiling\n' "$GRANT_ULXC" >&2
  printf '  (DefaultAgentCeilingLXC = %s µLXC = 50 LXC). The single key minted here cannot\n' "$CEILING_ULXC" >&2
  printf '  spend past the ceiling — the excess is stranded until an operator raises it\n' >&2
  printf '  (store-level only; no HTTP route). Proceeding anyway.\n' >&2
fi

# ── HTTP helper: sets RESP_CODE + RESP_BODY, never fails on non-2xx ──────────
req() { # method path [json-body]
  local out
  if [ $# -ge 3 ]; then
    if ! out=$(curl -sS --max-time 30 -X "$1" "$LENS_URL$2" \
        -H "Authorization: Bearer $LENS_API_KEY" \
        -H 'Content-Type: application/json' \
        -d "$3" -w $'\n%{http_code}'); then
      die "request failed: $1 $2 — is the SSH tunnel to $LENS_URL up? (docs/remote-host.md §4)"
    fi
  else
    if ! out=$(curl -sS --max-time 30 -X "$1" "$LENS_URL$2" \
        -H "Authorization: Bearer $LENS_API_KEY" -w $'\n%{http_code}'); then
      die "request failed: $1 $2 — is the SSH tunnel to $LENS_URL up? (docs/remote-host.md §4)"
    fi
  fi
  RESP_CODE=${out##*$'\n'}
  RESP_BODY=${out%$'\n'*}
  RESP_BODY=${RESP_BODY%$'\n'}
}

# ── preflight ────────────────────────────────────────────────────────────────
curl -fsS --max-time 5 "$LENS_URL/healthz" >/dev/null 2>&1 \
  || die "no Lens answering at $LENS_URL/healthz — is the SSH tunnel up? (docs/remote-host.md §4)"

# Refuse an existing workspace (see NOT-idempotent note in the header).
req GET "/v1/workspaces/$WS_ID"
case "$RESP_CODE" in
  (404) : ;; # fresh id — good
  (200) die "workspace '$WS_ID' already exists — refusing to continue (re-create would reset its config via the blind upsert; re-grant would double-fund). Pick a new id, or run the individual curls from docs/remote-host.md §5." ;;
  (401) die "admin auth rejected (401 invalid API key) — LENS_API_KEY wrong, or unset on the server (docs/remote-host.md §4)" ;;
  (*)   die "unexpected HTTP $RESP_CODE probing workspace '$WS_ID': $RESP_BODY" ;;
esac

# ── 1/3 create the workspace ─────────────────────────────────────────────────
req POST "/v1/workspaces" '{"id":"'"$WS_ID"'","name":"'"$WS_NAME_JSON"'"}'
[ "$RESP_CODE" = 201 ] || die "create workspace failed (HTTP $RESP_CODE): $RESP_BODY"
printf '✔ 1/3 workspace %s created\n' "$WS_ID"

# ── 2/3 mint the proxy-scoped key ────────────────────────────────────────────
req POST "/v1/workspaces/$WS_ID/api-keys" '{"name":"'"$WS_ID"'-trial","scopes":["proxy"]}'
[ "$RESP_CODE" = 201 ] || die "mint key failed (HTTP $RESP_CODE): $RESP_BODY"
WS_KEY=$(printf '%s' "$RESP_BODY" | sed -n 's/.*"key"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
[ -n "$WS_KEY" ] || die "could not parse \"key\" from mint response: $RESP_BODY"
case "$WS_KEY" in
  (tlv_ws_*) : ;;
  (*) die "minted key does not start with tlv_ws_ — refusing to hand it out (server response shape changed?)" ;;
esac
printf '✔ 2/3 key minted (scope: proxy) — shown ONCE at the end of this output\n'

# ── 3/3 fund it ──────────────────────────────────────────────────────────────
req POST "/v1/admin/lxc/grant" \
  '{"workspace_id":"'"$WS_ID"'","amount_ulxc":'"$GRANT_ULXC"',"reason":"trial comp: '"$WS_ID"'"}'
case "$RESP_CODE" in
  (200) : ;;
  (404) die "grant endpoint not registered (404): set LENS_ADMIN_LXC_GRANT_ENABLED=true in the server .env and 'docker compose up -d lens' (docs/remote-host.md §5, .env.production.example) — the workspace and key above WERE created; re-run with a NEW id after fixing, or grant manually" ;;
  (401) die "admin auth rejected at the grant step (401) — the workspace and key above WERE created; grant manually once LENS_API_KEY is fixed" ;;
  (*)   die "grant failed (HTTP $RESP_CODE): $RESP_BODY — the workspace and key above WERE created; grant manually" ;;
esac
NEW_BAL_ULXC=$(printf '%s' "$RESP_BODY" | sed -n 's/.*"new_balance_ulxc"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p')
[ -n "$NEW_BAL_ULXC" ] || NEW_BAL_ULXC="$GRANT_ULXC"

GRANT_LXC=$(awk "BEGIN{printf \"%.2f\", $GRANT_ULXC/1000000}")
GRANT_USD=$(awk "BEGIN{printf \"%.2f\", $GRANT_ULXC/1000000*0.10}")
BAL_LXC=$(awk "BEGIN{printf \"%.2f\", $NEW_BAL_ULXC/1000000}")
# shellcheck disable=SC2016 # the $%s / $0.10 are literal dollar amounts, not expansions
printf '✔ 3/3 granted %s µLXC = %s LXC ($%s at the $0.10 peg) — new balance %s LXC\n' \
  "$GRANT_ULXC" "$GRANT_LXC" "$GRANT_USD" "$BAL_LXC"

# ── hand-off ─────────────────────────────────────────────────────────────────
cat <<EOF

────────────────────────────────────────────────────────────────────────────────
Workspace key for '$WS_ID' — shown ONCE, never stored by this script.
Hand it over via a password manager, never chat:

    $WS_KEY

Client setup for the user (any OpenAI/Anthropic-compatible client):

    curl $LENS_PUBLIC_URL/v1/proxy/openai/v1/chat/completions \\
      -H "Authorization: Bearer $WS_KEY" \\
      -H 'Content-Type: application/json' \\
      -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
────────────────────────────────────────────────────────────────────────────────
Done onboarding everyone? Remove LENS_API_KEY from the server .env and
'docker compose up -d lens' again → admin fails closed (docs/remote-host.md §4).
EOF
