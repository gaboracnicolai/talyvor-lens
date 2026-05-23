# Talyvor Lens — Quick Start

End-to-end setup in under five minutes. By the end of this guide you'll have Lens running locally, an issued API key, and a verified working proxy request against OpenAI.

## Prerequisites

- Docker and Docker Compose installed (Docker Desktop on macOS / Windows; `docker.io` + `docker-compose-plugin` on Linux).
- At least one LLM provider API key (OpenAI, Anthropic, Google, Mistral, Groq, or AWS Bedrock).
- A terminal with `curl` and `jq` (optional — for pretty-printing JSON responses).

## Step 1 — Configure

```bash
cp .env.production.example .env
```

Edit `.env`. The minimum to bring up Lens with a working provider:

```bash
LENS_OPENAI_API_KEY=sk-...
POSTGRES_PASSWORD=changeme-in-production
```

Set additional provider keys (Anthropic, Google, Mistral, Groq) if you want to use them. Each missing key just disables that provider's route — it doesn't block startup.

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
| `lens` container restarting | Postgres init failed | `docker compose logs migrate` |
| `503 Service Unavailable` from `/v1/proxy/openai/*` | `LENS_OPENAI_API_KEY` missing in `.env` | Set the key, `docker compose up -d` |
| `401 Unauthorized` from proxy | Wrong Lens API key | `curl /v1/api/keys` to list, regenerate if needed |
| Dashboard shows no data | First request hasn't fired yet | Send a test request via curl |
| Status page shows red | One of postgres/redis/nats is down | `docker compose ps`, restart the offender |
