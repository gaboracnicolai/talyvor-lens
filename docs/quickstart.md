# Talyvor Lens — Quick Start

End-to-end setup in under five minutes. By the end of this guide you'll have Lens running locally, an issued API key, and a verified working proxy request against OpenAI.

## Prerequisites

- Docker and Docker Compose installed (Docker Desktop on macOS / Windows; `docker.io` + `docker-compose-plugin` on Linux).
- **Both** an OpenAI **and** an Anthropic API key — Lens requires `LENS_OPENAI_API_KEY` **and** `LENS_ANTHROPIC_API_KEY` to start (see Step 1). If you only use one provider, a dummy non-empty value satisfies the boot check for the other. The remaining providers (Google, Mistral, Groq, AWS Bedrock) are optional.
- A terminal with `curl` and `jq` (optional — for pretty-printing JSON responses).

## Step 1 — Configure

```bash
cp .env.production.example .env
```

Edit `.env`. The minimum to bring up Lens:

```bash
LENS_OPENAI_API_KEY=sk-...            # real — this guide proxies to OpenAI in Step 4
LENS_ANTHROPIC_API_KEY=sk-dummy       # required to boot; a dummy non-empty value is fine if you don't use it
POSTGRES_PASSWORD=changeme-in-production
```

**Both `LENS_OPENAI_API_KEY` and `LENS_ANTHROPIC_API_KEY` are mandatory.** `config.Load` (`internal/config/config.go`) hard-requires both and refuses to start with `ErrMissingEnv` if *either* is empty — this is **not** "at least one". And because `docker-compose.yaml` defaults these two with `:-` (empty) rather than `:?`, an unset key does **not** fail `docker compose up`; instead the `lens` container comes up and **crash-loops**. The symptom is a `lens` service stuck restarting — `docker compose logs lens` shows `missing required environment variables: [LENS_ANTHROPIC_API_KEY]`. A dummy non-empty string satisfies the boot check for a provider you don't actually call.

The *other* providers (Google, Mistral, Groq, AWS Bedrock) really are optional: a missing key there only disables that provider's `/v1/proxy/*` route (503) and does not block startup.

For the full set of first-boot traps, see [local-standup-runbook.md](local-standup-runbook.md) (Trap 3, which documents this exact requirement).

## Step 2 — Start

```bash
docker compose up -d
```

Wait ~10 seconds for healthchecks to settle. Verify:

```bash
docker compose ps
```

All five services (`lens`, `postgres`, `redis`, `nats`, `migrate`) should show healthy or completed. The `migrate` service exits 0 after applying schema; that's normal.

Verify the proxy is up:

```bash
curl -s http://localhost:8080/healthz
# {"ok":true}
```

And the public status page:

```bash
curl -s http://localhost:8080/status.json | jq .status
# "operational"
```

## Step 3 — Create your first API key

```bash
curl -X POST http://localhost:8080/v1/api/keys \
  -H "Content-Type: application/json" \
  -d '{"workspace_id":"default","name":"my-first-key"}'
```

The response includes the raw key once. Save it — it's never shown again.

```json
{
  "id": "key_...",
  "raw": "tlv_...",
  "workspace_id": "default",
  "name": "my-first-key"
}
```

Export it for the next steps:

```bash
export LENS_KEY=tlv_paste_yours_here
```

## Step 4 — Make your first proxied request

```bash
curl http://localhost:8080/v1/proxy/openai/v1/chat/completions \
  -H "Authorization: Bearer $LENS_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role":"user","content":"Hello!"}]
  }'
```

Lens forwards the request to OpenAI using `LENS_OPENAI_API_KEY` from your `.env`, caches the response, scores it, and records the cost. The response is OpenAI-format unchanged.

## Step 5 — View your dashboard

Open `http://localhost:8080/dashboard` in a browser. You'll see:

- Spend summary (your one test request)
- Cache hit rate (currently 0% — one request, one miss)
- Workspace activity
- Anomaly scan (empty — no anomalies yet)

Send the same request again. Refresh the dashboard. Cache hit rate jumps to 50% — the second request was served from the exact cache, no upstream API call.

## Step 6 — Check your savings

```bash
curl -H "Authorization: Bearer $LENS_KEY" \
  http://localhost:8080/v1/api/spend/summary
```

After a few requests the summary shows total cost, cached request count, and the dollars Lens saved you by serving from cache.

## Next steps

- **Set up per-workspace logging policy** — `PUT /v1/workspaces/{wsID}/logging` to switch a workspace to `metadata` (no prompt text persisted) or `none` (privacy mode).
- **Configure guardrails** — `PUT /v1/guardrails/policy` to enable PII redaction, prompt-injection blocking, blocked topics, or custom regex rules per workspace.
- **Add a prompt template** — `POST /v1/prompts` to register a versioned prompt; reference it from your application as `lens:prompt:<name>`.
- **Wire fallback chains** — `PUT /v1/api/fallback/chains/{provider}` to control which providers Lens falls over to when the primary fails.
- **Export audit logs** — `GET /v1/audit/export?format=ndjson` streams the audit log directly into your SIEM.
- **Migrate existing clients** — see [migrate-from-helicone.md](migrate-from-helicone.md) or [migrate-from-litellm.md](migrate-from-litellm.md).

## Stopping + cleanup

```bash
make down     # stop services, keep data volumes
make reset    # stop services and DELETE data volumes (fresh start)
```

Or directly:

```bash
docker compose down              # stop, keep volumes
docker compose down -v           # stop and wipe volumes
```

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `lens` container restarting / crash-looping | `LENS_OPENAI_API_KEY` or `LENS_ANTHROPIC_API_KEY` unset — `config.Load` requires **both** (compose defaults them with `:-`, so `up` succeeds but lens exits) | `docker compose logs lens` → `missing required environment variables`; set both in `.env`, then `docker compose up -d` |
| `lens` container restarting | Postgres init failed | `docker compose logs migrate` |
| `503 Service Unavailable` from `/v1/proxy/openai/*` | `LENS_OPENAI_API_KEY` is invalid or a dummy value (a *missing* one crash-loops the container instead — see the row above) | Set a real key, `docker compose up -d` |
| `401 Unauthorized` from proxy | Wrong Lens API key | `curl /v1/api/keys` to list, regenerate if needed |
| Dashboard shows no data | First request hasn't fired yet | Send a test request via curl |
| Status page shows red | One of postgres/redis/nats is down | `docker compose ps`, restart the offender |
