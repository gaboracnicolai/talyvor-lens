# Trial harness

The operator rig that drives synthetic traffic through a local full-stack deployment and verifies the
resulting DB state. Two scenario families:

- **Routing-pattern EARNING (a–j)** — the original S1–S4 trial; surfaced the deploy-integrity bugs fixed in
  #125–#129 and #133.
- **Semantic-isolation + authz + JWT (k–r, PR 2a)** — demonstrates the #142 private-cache `workspace_id`
  boundary end-to-end (the proof #159 could not run until #164 made the embedder endpoint configurable),
  plus an authz smoke test and the JWT-fallback closure.
- **DISTILL economy (n–p, PR 2b)** — the cross-tenant pooled-distill serve through the distill-worker
  subprocess: the three-switch consent matrix, the `distill_serve_attribution` counter, and the admin read
  (#166). Needs the `docker-compose.trial-distill.yaml` overlay + a document fixture.

**Neutrality of the shared scripts:** `seed.sh`/`traffic.sh` only gained ADDITIVE helpers + workspaces
(`distillpolicy`/`distillpoolable`/`send-doc`/`clear-requester-caches`; `ws-distill-*`). The (a)–(m)
provisioning, `send`/`send-jwt`/`get`, and the pattern/semantic workspaces are byte-unchanged, and the
distill flag lives in a SEPARATE overlay (absent from the (a)–(m) runs) — so existing scenarios are
unaffected. The distiller is inert for them regardless: a–m never set `distill_policy`/`distill_poolable`.

> **Discipline (read first):** this is a SYNTHETIC trial with valueless tokens and throwaway workspaces. It
> is evidence-gathering — it **satisfies NOTHING on the flip-on checklist**, which gates real-customer flips
> of `LENS_PATTERN_EARNING_ENABLED`. Gate (d) Sybil controls and the external audit remain open regardless of
> trial outcome.

## Contents
- `mock_vllm.py` — deterministic OpenAI-compatible upstream. `/v1/chat/completions` returns stable response
  bytes → stable content-hash `request_id`; sub-millisecond → pins `latency_bucket=fast`. `/v1/embeddings`
  returns 1536-dim vectors with an engineered collision: any prompt containing **`TLVCOLLIDE`** → one fixed
  vector (cosine 1.0 between any two marker prompts, ≥ the 0.92 threshold), every non-marker prompt → a
  vector that is **exactly** cosine-0 to the marker (axis-0 pinned) so it can never spuriously match.
- `seed.sh` — provisions workspaces (`tlv_` keys, opt-ins, `cache_poolable` consent, a minted JWT), writes
  `keys.tsv` (gitignored).
- `traffic.sh` — `send <ws> <model> <prompt>` / `admin-send` / `send-jwt <jwt-label> <model> <prompt>` /
  `get <ws> <path>`.
- `../../docker-compose.trial.yaml` — the explicit trial overlay (flags + mock service); **not**
  auto-loaded.

## Setup
Run everything from the repo root.

```bash
cp .env.production.example .env
echo "LENS_API_KEY=$(openssl rand -hex 32)" >> .env        # admin bootstrap key; .env is gitignored

# TRIAL-ONLY EC P-256 JWT signing key for scenario (r). Synthetic + valueless;
# kept in the shell env (NOT committed — multi-line PEM). Omit it and (r) is
# skipped, exactly as the legacy (h) gap behaved.
export LENS_JWT_PRIVATE_KEY="$(openssl ecparam -genkey -name prime256v1 -noout)"

docker compose -f docker-compose.yaml -f docker-compose.trial.yaml up -d --build  # builds lens; migrate applies 0001..N

export LENS_API_KEY=$(grep '^LENS_API_KEY=' .env | cut -d= -f2)
bash tools/trial/seed.sh                                   # provisions workspaces -> tools/trial/keys.tsv
bash tools/trial/traffic.sh send ws-base trial-base "hello-$(uuidgen)"
```

> The semantic scenarios (k–m) need a lens image built from a commit at/after #164 (`LENS_EMBEDDING_BASE_URL`
> support) — hence `--build`. The overlay sets that var to the mock's **full** `/v1/embeddings` path (the
> embedder uses the value verbatim, like the OpenAI const, and does not append a path).

Verify state with `psql` against the `postgres` container (`docker compose exec -T postgres psql -U lens -d
talyvor_lens -c '…'`). Three flags must be on: `LENS_PATTERN_MINING_ENABLED` (gates the opt-in route),
`LENS_PATTERN_CAPTURE_ENABLED`, `LENS_PATTERN_EARNING_ENABLED`. Use a distinct `model` per scenario so the
rarity tuple `(model, provider, input_bucket, latency_bucket)` doesn't bleed across scenarios.

## Scenario checklist (expected DB state)
- **(a) base earn** — solo opted-in workspace, one scored request → 1 `routing_patterns` row (rarity 0,
  earned **0.001**) + 1 `pattern_mine_credits` + 1 `lens_token_ledger` (type `pattern_mine`) + balance 0.001.
  The corroboration floor zeroes the *premium*, not the payout.
- **(b) corroboration premium** — 4 opted-in workspaces, **same tuple, sequential** → ws1–3 earn 0.001
  (n<3), **ws4 earns 0.002** (n=3 → rarity 0.25 → ×2.0); earlier rows are never re-priced.
- **(c) claim idempotency** — replay byte-identical work → `ON CONFLICT (request_id, workspace_id) DO
  NOTHING` → exactly one credit ever. (Redis exact-cache short-circuits before the seam; `FLUSHALL` between
  sends to exercise the DB-level claim. **NOTE since PR 2a:** the overlay sets `LENS_EMBEDDING_BASE_URL`,
  which makes the *semantic* cache (Postgres `prompt_embeddings`) live too — so the replay now ALSO
  semantic-hits. Clear BOTH stores between sends — they are SEPARATE: `redis-cli FLUSHALL` clears the Redis
  exact cache, and `psql -c 'TRUNCATE prompt_embeddings;'` clears the Postgres semantic cache (`FLUSHALL`
  does NOT touch `prompt_embeddings`). Only this scenario is affected; a/b/d–j use distinct prompts →
  semantic miss.)
- **(d) cap** — cap=3/2m, burst 5 distinct-prompt requests → exactly **3** earn rows == 3 claims == 3 ledger
  rows (the no-orphan invariant), balance 0.003; wait out the window → a 6th re-earns (0.004).
- **(e) exclusions** — non-opted-in → **zero** rows (capture is consent-gated); LoggingNone → nothing incl.
  `token_events`; the global admin key (WorkspaceID "") → serves 200, no earn.
- **(f) endpoints** — `GET /v1/tokens/rates` → `.pattern == {base_per_pattern:0.001, rarity_multiplier_max:2}`
  (dropped keys absent); `GET /v1/workspaces/{ws}/tokens/mining/patterns` → `total_earned` reconciles with
  `SUM(pattern_mine_credits)` / the ledger (not `patterns_shared`, which counts capture rows).
- **(g) auth fast path** — a `tlv_` workspace key earns through the full `AuthMiddleware → GetAPIKey` chain.
- **(h) auth fallback** — a JWT-workspace credential should also earn; **untested** unless
  `LENS_JWT_PRIVATE_KEY` is configured (a pre-flip test item for whichever deployment first enables JWT).
- **(i) Sybil friction (measurement)** — from one seed credential, loop mint-key + opt-in to provision N
  earning workspaces; record wall-clock + request count; `POST /v1/workspaces` is skippable (no FK). Feeds
  the gate-(d) Sybil-controls design.
- **(j) ops (measurement)** — TTLB A/B of flags-off vs on+opted-in vs on+non-opted-in over ~100 small
  requests (earn is post-flush → ~0 client delta); watch `pg_stat_activity` under a concurrent burst (the
  earn path adds an `IsOptedIn` read + a single-tx credit per request).

## PR 2a scenarios — semantic isolation + authz + JWT

`PSQL` below = `docker compose exec -T postgres psql -U lens -d talyvor_lens`. Marker prompts contain
`TLVCOLLIDE` (forced embedding collision); the model `sem-iso` keeps these off the pattern workspaces. Start
each from a clean cache: `docker compose exec -T redis redis-cli FLUSHALL && PSQL -c 'TRUNCATE prompt_embeddings;'`.

- **(k) cross-tenant BOUNDARY (the #142 proof).** Two tenants, colliding embeddings, must NOT cross.
  ```
  bash tools/trial/traffic.sh send ws-sem-a sem-iso "TLVCOLLIDE alpha"   # owner populates a private row
  bash tools/trial/traffic.sh send ws-sem-b sem-iso "TLVCOLLIDE beta"    # collides on embedding (cosine 1.0)
  PSQL -c "SELECT workspace_id, is_poolable, count(*) FROM prompt_embeddings GROUP BY 1,2 ORDER BY 1;"
  ```
  ASSERT **two private rows, one per workspace** (`ws-sem-a|f|1`, `ws-sem-b|f|1`): despite the cosine-1.0
  collision, `ws-sem-b` MISSED `ws-sem-a`'s row (the `workspace_id` filter) and stored its OWN.

- **(l) inverse control — own-row HIT.** The boundary must not break same-tenant caching.
  ```
  bash tools/trial/traffic.sh send ws-sem-a sem-iso "TLVCOLLIDE alpha-again"   # marker → matches own row
  PSQL -c "SELECT workspace_id, count(*) FROM prompt_embeddings GROUP BY 1 ORDER BY 1;"
  ```
  ASSERT `ws-sem-a` STILL has exactly **1** row — a semantic HIT stores nothing (a miss would add a 2nd).
  Positive signal: `curl -s localhost:8080/metrics -H "Authorization: Bearer $LENS_API_KEY" | grep
  cache_hit_semantic` increments.

- **(m) pooled control — consented cross-tenant sharing still works.** Both `cache_poolable=true` (seeded);
  needs `LENS_CACHE_POOLABLE_ENABLED=true` (overlay).
  ```
  bash tools/trial/traffic.sh send ws-pool-a sem-iso "TLVCOLLIDE pooled-x"   # stores private + POOLED rows
  bash tools/trial/traffic.sh send ws-pool-b sem-iso "TLVCOLLIDE pooled-y"   # collides → pooled hit
  PSQL -c "SELECT workspace_id, is_poolable, coalesce(contributor_workspace_id,'-') FROM prompt_embeddings ORDER BY is_poolable;"
  ```
  ASSERT a pooled row (`is_poolable=t`, contributor `ws-pool-a`) exists and `ws-pool-b` stored **no** private
  row — it served `ws-pool-a`'s pooled artifact. Positive signal:
  `… | grep cache_hit_pooled_semantic` increments. (Contrast (k): the SAME request shape stored a row on the
  private-filtered miss; here it doesn't, because it hit the pooled cache.)

- **(q) authz smoke.**
  ```
  bash tools/trial/traffic.sh get ws-authz-a /v1/workspaces/ws-authz-b/tokens/history   # cross-tenant
  bash tools/trial/traffic.sh get ws-authz-a /v1/workspaces/ws-authz-a/tokens/history   # own
  bash tools/trial/traffic.sh get ws-authz-a /v1/admin/distill/attribution              # tenant on admin
  ```
  ASSERT cross-tenant → **403**, own → **200**, tenant-on-admin → **401** (admin key → **200**).
  `GET /v1/workspaces/{wsID}/tokens/history` carries a `{wsID}` path param, so it is gated by
  `workspaceIsolationMiddleware`, which returns **403** "forbidden: credential not authorized for this
  workspace" for any unauthorized workspace (pinned by `cmd/lens/workspace_authz_test.go`). This is the
  workspace-isolation convention and is DELIBERATELY DISTINCT from the object-id IDOR convention: reads keyed
  by an opaque object id (e.g. `GET /v1/sessions/{sessionID}`) return **404**, identical to a genuine
  not-found, so they leak no existence oracle (#152, pinned by `cmd/lens/authz_routes_phase3_test.go`). A
  workspace boundary is the tenant's own identity, not a hidden object — so 403 (not 404) is correct here.
  The 401 covers (p)'s admin-route-gate half.

- **(r) JWT fallback (closes the legacy (h) gap).** `seed.sh` mints `ws-jwt-jwt` via `/v1/auth/token` when
  `LENS_JWT_PRIVATE_KEY` is set.
  ```
  bash tools/trial/traffic.sh send-jwt ws-jwt-jwt sem-iso "jwt probe $(date +%s)"
  PSQL -c "SELECT workspace_id, count(*) FROM token_events WHERE workspace_id='ws-jwt' GROUP BY 1;"
  ```
  ASSERT the send → **200** and a `token_events` row under `ws-jwt` — the JWT authenticated through the
  `AuthMiddleware` fallback and resolved to its workspace. (A garbage bearer → 401: auth is enforced.)

## DISTILL economy scenarios (n)(o)(p) — needs the trial-distill overlay (#166)

Bring the stack up with **both** the PR 2a overlay and the distill overlay:
```
docker compose -f docker-compose.yaml -f docker-compose.trial.yaml -f docker-compose.trial-distill.yaml up -d
```
`seed.sh` provisions `ws-distill-a` (owner) + `ws-distill-b` (requester) — both `distill_policy=always` and
`distill_poolable=true` — plus `ws-distill-c` (`distill_poolable=false`). `FIX=tools/trial/fixtures/distill-fixture.pdf`
is a fixed 612-byte text PDF; its sha256 **`ce2075ac…`** IS the `content_hash` the assertions key on (do not
regenerate — see the builder's warning). `reset` = `PSQL -c 'TRUNCATE distill_serve_attribution; TRUNCATE
prompt_embeddings;' && redis-cli FLUSHALL`. `PSQL` = `docker compose exec -T postgres psql -U lens -d talyvor_lens`.

- **(n) three-switch matrix.** The cross-tenant pooled distill serve fires only when the global switch AND
  the requester AND the owner are all opted in (`MaybeAllowPooledHit`). Four legs, `reset` before each:
  - **both-on** → serve + attribution row:
    ```
    bash tools/trial/traffic.sh send-doc ws-distill-a $FIX   # owner writes the pooled artifact
    bash tools/trial/traffic.sh send-doc ws-distill-b $FIX   # requester serves it cross-tenant
    PSQL -c "SELECT owner_workspace_id, requester_workspace_id, left(content_hash,12), serve_count FROM distill_serve_attribution;"
    #  ws-distill-a | ws-distill-b | ce2075ac9aba | 1
    ```
  - **requester-off** (`PUT …/distill-poolable {"distill_poolable":false}` on b) → **0 rows** (requester not opted in).
  - **owner-off** (false on a, true on b) → **0 rows** (the owner wrote no pooled entry; the requester
    re-converts and may write its OWN pooled `:owner` entry, but no *cross-tenant* serve fires).
  - **global-off** (recreate lens with base + `trial.yaml` only, WITHOUT `trial-distill.yaml`) → **0 rows**,
    and **no** `lens:distill:*:owner` key at all (`DecidePoolableOnWrite` is false when the global switch is off).

- **(o) attribution counter + idempotent bump.** From the both-on state (`serve_count=1`):
  ```
  bash tools/trial/traffic.sh clear-requester-caches ws-distill-b   # exact + private-distill + semantic; KEEPS the pooled entry
  bash tools/trial/traffic.sh send-doc ws-distill-b $FIX
  PSQL -c "SELECT owner_workspace_id, requester_workspace_id, serve_count FROM distill_serve_attribution;"
  #  ws-distill-a | ws-distill-b | 2      (SAME row, bumped)
  ```
  **PROMINENT:** without `clear-requester-caches`, a byte-identical re-serve short-circuits in THREE caches —
  the Redis exact cache, the per-requester distill-conversion entry, and (because the PR 2a overlay sets
  `LENS_EMBEDDING_BASE_URL`) the Postgres semantic cache — BEFORE the pooled distill read, so `serve_count`
  does NOT move. That is **correct production behavior** (identical re-requests are cached), NOT a bug; the
  triple clear is what makes the bump observable. The helper preserves the pooled entry and its `:owner` key.

- **(p) admin-read data half.** With the admin key:
  ```
  curl -fsS "$BASE/v1/admin/distill/attribution?view=pairs" -H "Authorization: Bearer $LENS_API_KEY"
  #  [{"owner_workspace_id":"ws-distill-a","requester_workspace_id":"ws-distill-b","serves":2,"last_served_at":…}]
  ```
  shows the (owner, requester) pair (and raw rows without `?view=pairs`). The tenant→**401** gate half is
  scenario (q) (a tenant key on this same admin route) — together they are the full admin-read coverage.

Do **not** track `keys.tsv`, `.env`, or any run state — see `.gitignore`.
