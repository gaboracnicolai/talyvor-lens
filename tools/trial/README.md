# Earn-stack trial harness

The operator rig that validated the routing-pattern **earning** stack (S1–S4) end-to-end against a local
full-stack deployment, and surfaced the deploy-integrity bugs fixed in #125–#129 and #133. It drives
synthetic traffic through the real proxy → earn path and verifies the resulting ledger/credit/pattern rows.

> **Discipline (read first):** this is a SYNTHETIC trial with valueless tokens and throwaway workspaces. It
> is evidence-gathering — it **satisfies NOTHING on the flip-on checklist**, which gates real-customer flips
> of `LENS_PATTERN_EARNING_ENABLED`. Gate (d) Sybil controls and the external audit remain open regardless of
> trial outcome.

## Contents
- `mock_vllm.py` — deterministic OpenAI-compatible upstream (stable response bytes → stable content-hash
  `request_id` across replays; sub-millisecond → pins `latency_bucket=fast`). Pointed at via
  `LENS_VLLM_BASE_URL`.
- `seed.sh` — provisions the scenario workspaces (mints `tlv_` keys, opts the right ones in), writes
  `keys.tsv` (gitignored).
- `traffic.sh` — sends non-streaming proxy requests by workspace (`send <ws> <model> <prompt>`).
- `../../docker-compose.trial.yaml` — the explicit trial overlay (flags + mock service); **not**
  auto-loaded.

## Setup
Run everything from the repo root.

```bash
cp .env.production.example .env
echo "LENS_API_KEY=$(openssl rand -hex 32)" >> .env        # admin bootstrap key; .env is gitignored
docker compose -f docker-compose.yaml -f docker-compose.trial.yaml up -d   # migrate sidecar applies 0001..N

export LENS_API_KEY=$(grep '^LENS_API_KEY=' .env | cut -d= -f2)
bash tools/trial/seed.sh                                   # provisions workspaces -> tools/trial/keys.tsv
bash tools/trial/traffic.sh send ws-base trial-base "hello-$(uuidgen)"
```

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
  sends to exercise the DB-level claim.)
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

Do **not** track `keys.tsv`, `.env`, or any run state — see `.gitignore`.
