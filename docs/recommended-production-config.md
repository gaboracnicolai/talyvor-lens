# Recommended production config — deployment-dependent operational flags

These are **bucket (b)** of the operational-flag recon: non-economic, non-destructive
operational flags that **cannot be set in the current docker-compose test** because
they need infrastructure that isn't there yet (a real cluster, a read replica, TLS
endpoints, a deployed frontend). Document now; set them when Andrew's cluster lands.

The **safe, no-infra operational flags are already turned on in staging** —
`docker-compose.trial.yaml` (`LENS_WORKTIER_ENABLED`, `LENS_GUARDRAILS_ENABLED`,
`LENS_QUALITY_AUTO_RETRY`, `LENS_ROI_INCLUDE_ENGINEER_BREAKDOWN`). This doc covers
only the deployment-dependent ones + the explicit do-not-touch list.

All names verified against `internal/config/config.go` at main `2cc7cbb`.

---

## Deployment-dependent flags (set in production, NOT in compose)

### High availability — `LENS_HA_ENABLED`
- **Default:** false.
- **Requires:** **multiple Lens instances** + Redis (`LENS_REDIS_URL`, already required) for
  the instance registry, the shared rate limiter, and circuit-breaker gossip.
- **Recommended (prod):** `true` — once there are ≥2 replicas behind a load balancer.
- **Why not in compose:** the test runs a **single instance**; HA is safe to enable but
  **does nothing** with one instance (no peers to coordinate with). Enable it with the
  replica count, not before.

### Read replica — `LENS_DB_REPLICA_URL`
- **Default:** "" (unset → all reads use the primary, byte-identical).
- **Requires:** a **streaming read replica** DSN.
- **Recommended (prod):** the replica DSN, to offload analytics/display reads (forecast,
  cost-anomaly, admin distill-attribution). Money/authz/tx reads stay on the primary by
  construction. Replication lag is **observability-only** (it never gates routing).
- **Why not in compose:** there is **no replica** in the test stack; a malformed/unreachable
  replica falls back to the primary with a WARN, but there's nothing to point it at.

### TLS hardening — `LENS_REDIS_TLS` · `LENS_NATS_TLS` · `LENS_DB_SSL_MODE` · `LENS_TLS_DOMAIN` · `LENS_TLS_CACHE_DIR`
- **Defaults:** TLS off / `LENS_DB_SSL_MODE` permissive; `LENS_TLS_DOMAIN`/`LENS_TLS_CACHE_DIR` unset.
- **Requires:** **TLS-terminating endpoints** — a Redis and NATS that speak TLS, a Postgres
  with certs, and (for the gateway's own ACME) a real **domain** + a writable cert cache dir.
- **Recommended (prod):**
  - `LENS_REDIS_TLS=true`, `LENS_NATS_TLS=true` — once those services terminate TLS.
  - `LENS_DB_SSL_MODE=verify-full` — strongest Postgres TLS (verifies cert + hostname).
  - `LENS_TLS_DOMAIN=<gateway-fqdn>` + `LENS_TLS_CACHE_DIR=<persistent path>` — for
    automatic ACME certs on the gateway.
- **Why not in compose:** the test stack is **plaintext** Redis/NATS/Postgres on a private
  Docker network — turning TLS on would **break connectivity** (a TLS client can't speak to a
  non-TLS server). These belong to the TLS-terminated production topology.
- **⚠ Keep these OFF in production:** `LENS_REDIS_TLS_SKIP_VERIFY` · `LENS_NATS_TLS_SKIP_VERIFY`
  · `LENS_NODE_TLS_SKIP_VERIFY`. They **disable certificate verification** (accept any cert) —
  a dev-only escape hatch that defeats the point of TLS. They must stay `false` in any real
  deployment.

### CORS — `LENS_CORS_ALLOWED_ORIGINS`
- **Default:** restrictive (no cross-origin).
- **Requires:** the **actual deployed frontend origin(s)**.
- **Recommended (prod):** the exact origin(s) the dashboard is served from (e.g.
  `https://app.talyvor.com`). Do **not** use `*`.
- **Why not in compose:** there is no deployed frontend origin in the test stack to allow.

---

## `LENS_ROI_INCLUDE_ENGINEER_BREAKDOWN` — a real-deployment checkbox

Enabled in **staging on synthetic/test data** (bucket a). It adds a **per-engineer (named
author)** cost breakdown to ROI reports — a cost *attribution*, not a performance judgment.

> **Real deployment note:** pointed at **named employees**, per-person cost tracking is
> personal-data processing. A real UK deployment carries an **employment / data-protection
> transparency consideration**: a documented lawful basis and **staff notice** before
> per-person cost attribution is shown to managers. Treat enabling it on real employee data
> as a deliberate, notified decision — not a default.

---

## Explicitly DO NOT touch (here)

- **All economic / pooling / LXC / routing-intelligence flags** — these are the **token
  economy**, gated by their own default-false flags + the `LENS_ECONOMY_ENABLED` kill-switch,
  and are a **separate go-live** (external audit, legal, etc.). Not operational:
  `LENS_POOL_ROYALTY_MINTING_ENABLED`, `LENS_POVI_MINTING_ENABLED`,
  `LENS_TRUSTFUL_COMPUTE_MINT_ENABLED`, `LENS_PATTERN_{MINING,CAPTURE,EARNING}_ENABLED`,
  `LENS_CACHE_SHARING_ENABLED`, `LENS_CACHE_POOLABLE_ENABLED`, `LENS_DISTILL_POOLABLE_ENABLED`,
  `LENS_ROUTING_INTELLIGENCE_ENABLED` (this one is in the economy kill-switch block — *not*
  an operational flag despite the name), `LENS_LXC_GATING_ENABLED`,
  `LENS_LXC_SHADOW_SPEND_ENABLED`, `LENS_BILLING_ENABLED` + the Stripe keys. (Staging turn-on
  for the distill economy is its own runbook: `docs/staging-economy-turnon.md`.)

- **`LENS_AUDIT_RETENTION` — leave UNSET.** Default = disabled (`≤0`/unset → the sweeper is
  off, so **nothing is ever pruned**). This is the one operational area with a **data-loss
  footgun**: setting a retention window prunes `token_events` by **age**, and with
  `LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE=false` (the default) it does **not** consult the
  export watermark — so a short window could delete **un-exported** audit history. If you ever
  want retention, set it **only** together with `LENS_AUDIT_REQUIRE_EXPORT_BEFORE_PRUNE=true`
  **and** a working `LENS_AUDIT_EXPORT_URL` sink (export-then-prune; PR #214 / U14 #187).

---

## Available but intentionally OFF (set later if you want them)

Not enabled now; both are safe to turn on whenever you choose:

- **`LENS_AUDIT_EXPORT_URL`** (+ `LENS_AUDIT_EXPORT_INTERVAL`, default 1h) — off-box export of
  `token_events` to a SIEM/sink. **Additive — deletes nothing.** Set it if you want an
  off-box audit trail (and it's the prerequisite for ever enabling retention safely, above).
- **`LENS_GLOBAL_RPM` / `LENS_GLOBAL_TPM`** (default `0` = no global cap) — an optional global
  rate ceiling on top of the per-workspace tiers. Set a value if you want a global throttle;
  protective, non-economic.
