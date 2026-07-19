# Local Standup Runbook — first serve, first mint

The exact, verified sequence to bring Lens up **standalone on one machine** and take it from zero to a
**real served request that writes a real `lens_token_ledger` row** (a `pattern_mine_held` mint). This
is what actually happened the first time Lens was stood up outside tests — every step here is followed
by *what to check*, and every trap below cost real time that night.

**Scope of this doc.** Local/self-hosted `docker compose`. All **H5 flags and cache/distill pooling
stay DEFAULT-OFF** — this runbook does **not** turn any of them on. It turns on exactly three feature
flags (two pattern flags + the admin-grant flag) and nothing else. `/healthz` being green proves the
process is alive and the DB/Redis are reachable — **not** that it can authenticate, serve a provider
request, or mint. Read the traps.

## Prerequisites

- Docker + Docker Compose.
- `git`, `curl`, and `jq`.
- A **real** Anthropic API key (the minting request proxies a live Anthropic completion). A real
  OpenAI key is *also* required just to boot (see Trap 3) — a dummy value is fine there.

---

## Part A — bring Lens up

### Step 0 — start from current code (Traps 1 & 2)

```bash
git pull                    # a behind-HEAD checkout builds the WRONG code
docker compose build lens   # compose prefers image: over build:; the ghcr `latest` tag lagged HEAD
                            # by 31 migrations once and minted with pre-SEC-2 FLOAT money code
```

**Check** — after `make up` in Step 2, confirm the running schema matches the source tree:

```bash
# both numbers must be EQUAL
ls migrations/*.sql | wc -l
docker compose exec -e PGPASSWORD="$POSTGRES_PASSWORD" postgres \
  psql -U lens -d talyvor_lens -tAc "SELECT count(*), max(version) FROM schema_migrations"
```

If the counts differ, you are running a stale image — rebuild (`docker compose build lens`) and
re-run the migrate step (`docker compose up -d migrate`).

### Step 1 — `.env` (Trap 3)

```bash
cp .env.production.example .env
```

Edit `.env`. **All three of these are mandatory** — `config.Load()` hard-requires **both** provider
keys (`internal/config/config.go`, the `missing` block), not "at least one":

```dotenv
POSTGRES_PASSWORD=pick-a-strong-local-password
LENS_ANTHROPIC_API_KEY=sk-ant-...        # REAL — the minting request calls Anthropic
LENS_OPENAI_API_KEY=sk-dummy             # required to boot; unused here, a dummy string is fine
```

`.env` is gitignored. `POSTGRES_PASSWORD` uses compose `:?` and fails `docker compose` loudly if
unset; the provider keys use `:-`, so **compose starts fine and lens crash-loops** if either is
missing. Watch for `missing required environment variables` in `docker compose logs lens`.

### Step 2 — start the stack

```bash
make up            # docker compose up -d  (the one-shot `migrate` service runs first, then lens)
```

**Check** — required services healthy, and lens actually up:

```bash
docker compose ps                 # postgres/pgbouncer/redis healthy; migrate = Exited (0); lens = Up (healthy)
curl -fsS localhost:8080/healthz  # 200  (DB+Redis reachable — NOT proof it can serve or mint; Trap 4)
```

---

## Part B — make it able to mint

Lens serves happily long before it can mint. The next steps clear every silent-zero gate.

### Step 3 — admin bootstrap key (`LENS_API_KEY`)

The admin key is read directly by `auth.NewManager(os.Getenv("LENS_API_KEY"), …)` (`cmd/lens/main.go`)
and is **not** in the tracked compose. Add it — plus the three feature flags used below — via a
`docker-compose.override.yml` that compose auto-merges. **Gitignore it first (it holds a secret; the
repo does not ignore it by default):**

```bash
echo 'docker-compose.override.yml' >> .gitignore

cat > docker-compose.override.yml <<'YAML'
services:
  lens:
    environment:
      - LENS_API_KEY=pick-a-strong-admin-key
      - LENS_PATTERN_MINING_ENABLED=true    # gates the opt-in ROUTE   (Trap 5)
      - LENS_PATTERN_EARNING_ENABLED=true   # gates the actual EARNING (Trap 5)
      - LENS_ADMIN_LXC_GRANT_ENABLED=true   # exposes the admin grant endpoint (Trap 7)
YAML

docker compose up -d lens
export ADMIN_KEY=pick-a-strong-admin-key    # same value, for the curls below
```

**Check** — an admin-only route now authenticates (empty `LENS_API_KEY` ⇒ admin auth is off):

```bash
curl -fsS -XPOST localhost:8080/v1/workspaces \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"id":"probe","name":"probe"}' && echo "  admin OK"
```

### Step 4 — create a workspace

The body needs an **`id`** (not `name` alone). `default` is auto-seeded but is excluded from pattern
minting (`internal/proxy/pattern_earn.go`), so use a fresh id.

```bash
export WS=trial-1
curl -fsS -XPOST localhost:8080/v1/workspaces \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d "{\"id\":\"$WS\",\"name\":\"Trial 1\"}"
```

**Check** — returns `{"id":"trial-1"}` (201).

### Step 5 — vouch it to earn (`earn_verified`, Trap 6)

`workspaces.earn_verified` defaults **false** (`migrations/0057_u6_sybil_floor.sql`) and there is **no
API and no Go setter** — it is a deliberate manual admin vouch (the U6 Sybil floor). Without it every
request 200s and **mints nothing**.

```bash
docker compose exec -e PGPASSWORD="$POSTGRES_PASSWORD" postgres \
  psql -U lens -d talyvor_lens -c \
  "UPDATE workspaces SET earn_verified=true WHERE id='$WS';"
```

**Check**:

```bash
docker compose exec -e PGPASSWORD="$POSTGRES_PASSWORD" postgres \
  psql -U lens -d talyvor_lens -tAc "SELECT id, earn_verified FROM workspaces WHERE id='$WS';"
# → trial-1|t
```

### Step 6 — opt the workspace into pattern earning

The opt-in route is gated by `LENS_PATTERN_MINING_ENABLED` (set in Step 3).

```bash
curl -fsS -XPOST "localhost:8080/v1/workspaces/$WS/pattern-mining/opt-in" \
  -H "Authorization: Bearer $ADMIN_KEY"
```

**Check**:

```bash
docker compose exec -e PGPASSWORD="$POSTGRES_PASSWORD" postgres \
  psql -U lens -d talyvor_lens -tAc "SELECT workspace_id FROM workspace_pattern_optin WHERE workspace_id='$WS';"
# → trial-1
```

### Step 7 — mint a workspace API key (`scopes` required)

The per-workspace key route is `/v1/workspaces/{ws}/api-keys` (hyphen). Give it the **`proxy`** scope —
the correct least-privilege scope for a key that will proxy completions.

```bash
export WS_KEY=$(curl -fsS -XPOST "localhost:8080/v1/workspaces/$WS/api-keys" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"standup","scopes":["proxy"]}' | jq -r '.key')
echo "$WS_KEY"     # a tlv_ws_... value, shown ONCE
```

**Check** — `$WS_KEY` starts with `tlv_ws_`.

### Step 8 — fund the workspace with a comped LXC grant (Trap 7)

A fresh workspace has **0 LXC**; the per-scoped-key agent sub-budget then fails a request with
"agent LXC sub-budget exceeded or insufficient balance". `/lxc/convert` has a 100000 µLXC floor
(`MinConversionLXC`), and without this grant the only funding path is a Stripe fiat purchase. The
admin grant (behind `LENS_ADMIN_LXC_GRANT_ENABLED`, set in Step 3) credits comped LXC through
`economy.GrantLXC` — the same atomic ledger+balance move as a purchase, but recorded under
`type='admin_grant'` so it is never mistaken for revenue (**Trap 8**).

```bash
curl -fsS -XPOST localhost:8080/v1/admin/lxc/grant \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d "{\"workspace_id\":\"$WS\",\"amount_ulxc\":100000000,\"reason\":\"local standup bootstrap\"}"
```

**Check** — returns `{"workspace_id":"trial-1","granted_ulxc":100000000,"new_balance_ulxc":100000000}`,
and the row is a grant, not a purchase:

```bash
docker compose exec -e PGPASSWORD="$POSTGRES_PASSWORD" postgres \
  psql -U lens -d talyvor_lens -tAc \
  "SELECT type, amount FROM lxc_ledger WHERE workspace_id='$WS';"
# → admin_grant|100000000
```

---

## Part C — serve, and prove the mint

### Step 9 — the request

```bash
curl -fsS -XPOST localhost:8080/v1/proxy/anthropic/v1/messages \
  -H "Authorization: Bearer $WS_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"claude-3-5-sonnet-20241022","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'
```

**Check** — a normal Anthropic `message` JSON (200). A `401` means the key/admin bootstrap is wrong;
a `5xx`/provider error means `LENS_ANTHROPIC_API_KEY` is dummy or invalid.

### Step 10 — the single proof query

```bash
docker compose exec -e PGPASSWORD="$POSTGRES_PASSWORD" postgres \
  psql -U lens -d talyvor_lens -c \
  "SELECT type, amount FROM lens_token_ledger WHERE workspace_id='$WS' AND type='pattern_mine_held' ORDER BY created_at DESC LIMIT 5;"
```

**PASS** = at least one row, `type = pattern_mine_held`, `amount >= 1000` (µLENS). That row is the
first real mint.

If it returns **zero rows** (the "it looked like it worked" case), walk the traps in order: earn_verified
true (Step 5)? · opted-in (Step 6)? · workspace ≠ `default` (Step 4)? · **both** pattern flags set
(Trap 5)? · `LENS_ECONOMY_ENABLED` not forced off? · did the completion actually 200 (Step 9)?

---

## The traps (each cost real time)

| # | Trap | Why it bites | Guard |
|---|------|--------------|-------|
| 1 | **Stale image** | The `lens` service declares BOTH `image:` and `build:`; compose prefers `image:`, so `make up` pulls ghcr `:latest` — once **31 migrations behind**, it booted clean, served, and minted with **pre-SEC-2 FLOAT** money code. | `docker compose build lens` first; verify `count(*)/max(version)` in `schema_migrations` == `ls migrations/*.sql | wc -l`. |
| 2 | **Stale local clone** | Even after building, a behind-HEAD checkout builds the wrong code. | `git pull` first. |
| 3 | **Missing provider key** | `config.Load()` hard-requires **both** `LENS_OPENAI_API_KEY` and `LENS_ANTHROPIC_API_KEY`. `POSTGRES_PASSWORD` uses compose `:?` (fails loudly); the provider keys use `:-`, so compose starts and **lens crash-loops**. | Set both in `.env` (a dummy OpenAI value boots). |
| 4 | **`/healthz` green ≠ can serve/mint** | It pings DB + Redis only — never auth, provider, or mint. | Treat green as "process + deps up", nothing more. |
| 5 | **The two pattern flags** | `LENS_PATTERN_MINING_ENABLED` gates only the opt-in **route**; `LENS_PATTERN_EARNING_ENABLED` gates the actual **earning**. Setting only MINING gives a successful opt-in and **silent zero** earnings. | Set **both**. |
| 6 | **`earn_verified` defaults false** | Without the manual vouch, every mint-type credit rolls back — request 200s, mints nothing. There is no API by design (U6). | `UPDATE workspaces SET earn_verified=true`. |
| 7 | **Bootstrap: 0 LXC** | A fresh workspace has 0 LXC; the agent sub-budget then rejects requests; `/lxc/convert` has a 100000 µLXC floor; without the admin grant the only funding path is Stripe. | `POST /v1/admin/lxc/grant` (behind `LENS_ADMIN_LXC_GRANT_ENABLED`). |
| 8 | **Ledger type matters** | `lxc_ledger` is **append-only** (`migrations/0055_audit_immutability.sql` guards it — never mutable/deletable). A comped grant hand-written as `type='purchase'` is **PERMANENT** and would be counted as revenue. **This mistake was made during the standup** — it is the reason the grant endpoint (which records `type='admin_grant'`) exists and hand-written SQL is banned. | Always use `POST /v1/admin/lxc/grant`, never raw `INSERT INTO lxc_ledger`. |

## What stays off

This runbook enables only `LENS_PATTERN_MINING_ENABLED`, `LENS_PATTERN_EARNING_ENABLED`, and
`LENS_ADMIN_LXC_GRANT_ENABLED`. **Every H5 flag** (`LENS_H5_ARTIFACT_ENABLED`, `_ATTEST_ENABLED`,
`_BONDS_ENABLED`) and **cache/distill pooling** stay at their default OFF. Do not turn them on to get a
mint — the pattern held-mint above needs none of them.
