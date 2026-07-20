# Remote host — putting Lens safely on the internet

The verified sequence to take the `docker compose` stack from a laptop to a **public VM reachable over
HTTPS**, with nothing exposed that shouldn't be. Lens self-authenticates its own `tlv_ws_` keys against
its database and reads **no** gateway-injected identity headers, so it is safe to sit directly behind a
plain reverse proxy — there is no header-spoofing trust to get wrong. Three things had to change from the
local posture; this doc is the result.

**Scope.** The internet-facing stack is `docker-compose.yaml`. `docker-compose.dev.yaml` and the
`tools/trial/*` overlays are **local-only** — do not run them on a public VM.

## What the front door looks like

```
internet ──443──▶  Caddy  ──http──▶  lens:8080        (Caddy terminates TLS; lens never sees the internet)
             ──80──▶  Caddy  (301 → 443)
                                lens :8080 also published on 127.0.0.1 only (loopback: local dev + admin)
                                postgres / pgbouncer / redis / nats  — internal compose network only
```

- **Caddy** terminates TLS on `:443` and reverse-proxies to `lens:8080` over the internal network. It
  provisions and auto-renews a Let's Encrypt certificate for your domain automatically. Chosen over
  Lens's built-in autocert (`cmd/lens/serve.go`, `LENS_TLS_DOMAIN`) because autocert needs a *writable*
  cert cache on the lens container, which would mean relaxing `read_only: true` — a hardening property
  worth keeping. Caddy owns its own cert store (the `caddy_data` volume); lens stays locked down.
- **Lens** is published on `127.0.0.1:8080` **only** — reachable from the VM's loopback (local checks,
  admin bootstrap over SSH) but never from the internet. The only internet path to Lens is through Caddy.
- **NATS, Postgres, PgBouncer, Redis** publish **no** host ports — they talk to Lens over the compose
  network. (NATS is an unauthenticated bus; before this it was published on `0.0.0.0:4222`.)

## 1. What you must supply

| You provide | Notes |
|---|---|
| A **VM** with Docker + Docker Compose | 2 vCPU / 4 GB is plenty to start. Ubuntu/Debian fine. |
| A **domain** (e.g. `lens.example.com`) | Any registrar. A subdomain is fine. |
| A **DNS A record** → the VM's public IP | Must resolve **before** first boot — Let's Encrypt validates over HTTP-01 on :80. |
| Firewall/security-group open on **:80 and :443** | :80 is needed for the ACME challenge + the 301 redirect. Nothing else needs to be open inbound. |
| A **provider key** (`LENS_OPENAI_API_KEY` and/or others) | At least one, or the proxy has nothing to forward. Both OpenAI **and** Anthropic keys must be present to boot (a dummy value satisfies the boot check for a provider you don't use). |
| A strong **`POSTGRES_PASSWORD`** | `openssl rand -hex 32`. The stack refuses to start without it. |

## 2. Bring-up

```bash
git clone <repo> && cd talyvor-lens
cp .env.production.example .env
# edit .env: set LENS_DOMAIN=lens.example.com, POSTGRES_PASSWORD, and a provider key.
docker compose up -d
```

That's it. On first boot Caddy obtains the certificate (a few seconds once DNS resolves) and migrations
apply automatically before lens starts. Leave `LENS_API_KEY` **unset** — see §4.

**What to check**

```bash
docker compose ps                     # every service Up/healthy; migrate Exited (0)
docker compose logs caddy | grep -i certificate   # "certificate obtained successfully"
```

## 3. Verify TLS

From anywhere:

```bash
curl -s https://lens.example.com/healthz            # 200 + a JSON health body
curl -s -o /dev/null -w '%{http_code}\n' http://lens.example.com/healthz   # 308 → https
```

Confirm the certificate is a real Let's Encrypt one and Lens is **not** reachable in the clear:

```bash
echo | openssl s_client -connect lens.example.com:443 -servername lens.example.com 2>/dev/null \
  | openssl x509 -noout -issuer            # issuer=C=US, O=Let's Encrypt, ...
curl -s --max-time 5 http://<VM-public-IP>:8080/healthz   # must FAIL (connection refused) — :8080 is loopback-only
```

(Locally you can dry-run the whole thing with `LENS_DOMAIN=localhost`; Caddy then serves an internal-CA
cert and `curl -k https://localhost/healthz` returns 200.)

## 4. The admin god-key (`LENS_API_KEY`) — leave it unset

Every `requireAdmin` route (`/v1/admin/*`, `/metrics`, workspace provisioning, …) is gated on
`LENS_API_KEY`. **Recommendation: leave it unset on the public host.** Admin then *fails closed* — with
no key, no request can ever be admin (`internal/auth/manager.go`: admin is granted only when the request
key exactly matches a **non-empty** `LENS_API_KEY`). This is a single, robust property that holds for
**every** admin route regardless of path — unlike a reverse-proxy path allowlist, which would have to
enumerate a surface (`/v1/admin/*`, `/metrics`, `POST /v1/workspaces`, `/ha/status`, …) and would
silently miss one.

We deliberately did **not** add a second internal-only listener for admin: that is a Go change, and the
loopback publish already gives a safe admin path (below).

**When you must run an admin action** (creating the first workspace, minting the first key), do it over
the loopback, never the public door:

```bash
# from your laptop, tunnel to the VM's loopback :8080
ssh -N -L 8080:127.0.0.1:8080 user@lens.example.com &
# temporarily set LENS_API_KEY in .env, then: docker compose up -d lens
curl -s -X POST http://127.0.0.1:8080/v1/workspaces \
  -H "Authorization: Bearer $LENS_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"id":"acme","name":"Acme"}'
# when done, remove LENS_API_KEY from .env and `docker compose up -d lens` again → admin inert.
```

If you would rather keep a persistent admin key, uncomment the `@admin` block in
`deploy/caddy/Caddyfile` so the admin prefix is refused at the public door (defense-in-depth; note the
caveat there that a couple of admin routes live outside `/v1/admin/*`).

## 5. Hand a colleague a workspace key

Workspace keys are per-tenant `tlv_ws_…` credentials; a colleague uses one as a normal bearer token
against `https://lens.example.com`. Bootstrap needs admin (over the tunnel from §4):

```bash
# 1. create the workspace (admin)
curl -s -X POST http://127.0.0.1:8080/v1/workspaces \
  -H "Authorization: Bearer $LENS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"id":"acme","name":"Acme"}'

# 2. mint a scoped key for it (admin). Give it the proxy scope so it can call /v1/proxy/*.
curl -s -X POST http://127.0.0.1:8080/v1/workspaces/acme/api-keys \
  -H "Authorization: Bearer $LENS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"alice-laptop","scopes":["proxy"]}'
# → {"key":"tlv_ws_…"}  ← returned ONCE. Hand THIS to your colleague (a password manager, not chat).
```

They then point any OpenAI/Anthropic client at the proxy:

```bash
curl https://lens.example.com/v1/proxy/openai/v1/chat/completions \
  -H "Authorization: Bearer tlv_ws_…" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

The key never grants admin, and a workspace can only ever see its own data.

## Port exposure — the full sweep

| File | Service | Host publish (after) | Why |
|---|---|---|---|
| `docker-compose.yaml` | **caddy** | `0.0.0.0:80`, `0.0.0.0:443` | the only internet-facing surface (TLS + ACME) |
| `docker-compose.yaml` | **lens** | `127.0.0.1:8080` | loopback only — local dev, `make status`, `tools/trial`, admin over SSH; **never** the internet |
| `docker-compose.yaml` | nats | *(none)* | was `0.0.0.0:4222` — an unauthenticated bus; now internal-only (`nats://nats:4222`) |
| `docker-compose.yaml` | postgres / pgbouncer / redis / autoheal | *(none)* | internal compose network only |
| `docker-compose.dev.yaml` | postgres / redis / nats | `127.0.0.1:5432 / 6379 / 4222 / 8222` | **local dev only** — loopback-bound so host tooling works but nothing is exposed if run on a VM. Do not run this file on a public host. |
| `docker-compose.trial*.yaml` | (overlays base) | inherits the base's `127.0.0.1:8080` | local test harness (`tools/trial`); publishes no ports of its own |

## Not covered here

The Helm chart under `deploy/helm/lens/` is **uninvoked scaffolding** with a placeholder image
(`ghcr.io/talyvor/talyvor-lens`, empty tag) — it is not part of this compose-based path and is not
wired up. Use compose for a single-VM host; the Helm chart would need real image/values work before it
runs, which is out of scope here.
