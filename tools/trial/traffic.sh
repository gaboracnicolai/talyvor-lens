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

send_doc() { # <ws> <pdf-path> — Anthropic document block carrying the PDF (distill n/o)
  local ws="$1" pdf="$2" k b64 body
  k=$(key_for "$ws"); [ -n "$k" ] || { echo "no key for $ws" >&2; return 2; }
  [ -f "$pdf" ] || { echo "no such pdf: $pdf" >&2; return 2; }
  b64=$(base64 < "$pdf" | tr -d '\n')
  body=$(python3 -c 'import json,sys; print(json.dumps({"model":"distill-doc","messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":sys.argv[1]}},{"type":"text","text":"summarize this document"}]}]}))' "$b64")
  curl -sS -o /tmp/lens_resp.json -w '%{http_code} %{time_total}s\n' \
    -X POST "$BASE/v1/proxy/vllm/v1/chat/completions" \
    -H "Authorization: Bearer $k" -H 'Content-Type: application/json' -d "$body"
}

# clear_requester_caches removes the three caches that short-circuit a
# BYTE-IDENTICAL re-serve BEFORE the pooled distill read, so scenario (o)'s
# serve_count bump becomes observable. DELETES (rig-wide, but pooled-preserving):
#   - lens:exact:*            the Redis exact cache
#   - lens:distill:<private>  per-requester distill-CONVERSION entries — every
#                             lens:distill:* that has NO ":owner" sibling
#   - prompt_embeddings rows  the Postgres semantic cache (live because the PR 2a
#                             overlay sets LENS_EMBEDDING_BASE_URL)
# MUST NEVER touch the POOLED entry — lens:distill:<poolmarker+hash> and its
# ":owner" sibling — that is the OWNER's shared artifact; deleting it would break
# the cross-tenant serve. The pooled base is the one that HAS a :owner key.
# (Uses `docker compose exec`, so run from the repo root.)
clear_requester_caches() { # <ws> (informational; the clear is rig-wide, pooled-preserving)
  docker compose exec -T redis sh -c '
    pooled=""
    for o in $(redis-cli --scan --pattern "lens:distill:*:owner"); do pooled="$pooled ${o%:owner}"; done
    for k in $(redis-cli --scan --pattern "lens:distill:*"); do
      case "$k" in *:owner) continue;; esac          # keep the :owner sibling
      case " $pooled " in *" $k "*) continue;; esac   # keep the pooled base
      redis-cli DEL "$k" >/dev/null
    done
    redis-cli --scan --pattern "lens:exact:*" | xargs -r redis-cli DEL >/dev/null' >/dev/null 2>&1
  docker compose exec -T postgres psql -U lens -d talyvor_lens -c "TRUNCATE prompt_embeddings;" >/dev/null 2>&1
  echo "  cleared exact + private-distill + semantic for ${1:-?} (pooled entry + :owner preserved)"
}

cmd="${1:-}"; shift || true
case "$cmd" in
  send)        send "$@" ;;
  admin-send)  admin_send "$@" ;;
  send-jwt)    send_jwt "$@" ;;
  get)         get "$@" ;;
  send-doc)    send_doc "$@" ;;
  clear-requester-caches) clear_requester_caches "$@" ;;
  *) echo "usage: traffic.sh {send <ws> <model> <prompt> | admin-send <model> <prompt> | send-jwt <jwt-label> <model> <prompt> | get <ws> <path> | send-doc <ws> <pdf-path> | clear-requester-caches <ws>}" >&2; exit 1 ;;
esac
