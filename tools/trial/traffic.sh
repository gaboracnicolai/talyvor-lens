#!/usr/bin/env bash
# Traffic driver for the routing-pattern earning trial. Sub-commands let an
# orchestrator interleave requests with psql checks.
#
#   Run from the repo root:
#     bash tools/trial/traffic.sh send <ws> <model> <prompt>   # one non-streaming POST
#     bash tools/trial/traffic.sh admin-send <model> <prompt>  # POST as the global admin key (WorkspaceID "")
#
# `send` prints  "<http_code> <time_total>s"  and leaves the body in /tmp/lens_resp.json.
# Distinct prompts defeat the per-workspace exact cache; the claim scenario
# deliberately reuses a prompt to force a replay.
set -euo pipefail
BASE=${BASE:-http://localhost:8080}
KEYS=${KEYS:-tools/trial/keys.tsv}
ADMIN=${LENS_API_KEY:-}

key_for() { awk -F'\t' -v w="$1" '$1==w{print $2}' "$KEYS"; }

body_for() { # <model> <prompt>
  python3 -c 'import json,sys; print(json.dumps({"model":sys.argv[1],"messages":[{"role":"user","content":sys.argv[2]}]}))' "$1" "$2"
}

send() { # <ws> <model> <prompt>
  local ws="$1" model="$2" prompt="$3" k
  k=$(key_for "$ws")
  [ -n "$k" ] || { echo "no key for $ws" >&2; return 2; }
  curl -sS -o /tmp/lens_resp.json -w '%{http_code} %{time_total}s\n' \
    -X POST "$BASE/v1/proxy/vllm/v1/chat/completions" \
    -H "Authorization: Bearer $k" -H 'Content-Type: application/json' \
    -d "$(body_for "$model" "$prompt")"
}

admin_send() { # <model> <prompt> — global admin key, WorkspaceID "" (must NOT earn)
  local model="$1" prompt="$2"
  curl -sS -o /tmp/lens_resp.json -w '%{http_code} %{time_total}s\n' \
    -X POST "$BASE/v1/proxy/vllm/v1/chat/completions" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "$(body_for "$model" "$prompt")"
}

send_jwt() { # <jwt-label> <model> <prompt> — auth via a minted JWT (scenario r)
  local label="$1" model="$2" prompt="$3" tok
  tok=$(key_for "$label")
  [ -n "$tok" ] || { echo "no jwt for $label (seed.sh needs LENS_JWT_PRIVATE_KEY)" >&2; return 2; }
  curl -sS -o /tmp/lens_resp.json -w '%{http_code} %{time_total}s\n' \
    -X POST "$BASE/v1/proxy/vllm/v1/chat/completions" \
    -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
    -d "$(body_for "$model" "$prompt")"
}

get() { # <ws> <path> — GET <path> as <ws>'s key; prints just the status (authz scenario q)
  local ws="$1" path="$2" k
  k=$(key_for "$ws")
  [ -n "$k" ] || { echo "no key for $ws" >&2; return 2; }
  curl -sS -o /tmp/lens_resp.json -w '%{http_code}\n' \
    "$BASE$path" -H "Authorization: Bearer $k"
}

cmd="${1:-}"; shift || true
case "$cmd" in
  send)       send "$@" ;;
  admin-send) admin_send "$@" ;;
  send-jwt)   send_jwt "$@" ;;
  get)        get "$@" ;;
  *) echo "usage: traffic.sh {send <ws> <model> <prompt> | admin-send <model> <prompt> | send-jwt <jwt-label> <model> <prompt> | get <ws> <path>}" >&2; exit 1 ;;
esac
