# Talyvor Lens

**AI token intelligence proxy — cut your LLM costs 60–80% by sitting between your app and the providers you already use.**

Drop-in replacement for OpenAI, Anthropic, Google Gemini, AWS Bedrock, Mistral, Groq, and vLLM. Change one URL. Get caching, routing, attribution, guardrails, audit, fallback, and a dashboard.

## Why Talyvor Lens?

| | Talyvor Lens | LiteLLM | Helicone |
|---|---|---|---|
| Language | Go | Python | Node.js |
| Single-binary deploy | ✅ | ❌ | ❌ |
| Semantic cache (pgvector) | ✅ | ❌ | ❌ |
| Exact cache (Redis) | ✅ | ✅ | ✅ |
| Idle memory | < 50 MB | ~300 MB | n/a (SaaS) |
| Supply chain | Clean | Compromised Mar 2026 | Acquired by Mintlify |
| Self-hosted | ✅ | ✅ | ❌ |
| Open source | ✅ (core) | ✅ | ✅ |
| Guardrails (PII / injection / topic / regex) | ✅ | partial | ❌ |
| MCP server | ✅ | ❌ | ❌ |
| Prompt versioning + rollback | ✅ | ❌ | ❌ |
| A/B model testing | ✅ | ❌ | ❌ |
| Cost anomaly detection | ✅ | ❌ | ❌ |
| AWS Bedrock (SigV4) | ✅ | ✅ | ❌ |

Numbers from public benchmarks and vendor docs. Reproduce with `make bench`.

## Quick start (2 commands)

```bash
# 1. Copy the env template and fill in at least one provider key
cp .env.production.example .env

# 2. Bring up the full stack (Lens + Postgres + Redis + NATS)
docker compose up -d
```

Lens is now running at `http://localhost:8080`.

Open the dashboard at `http://localhost:8080/dashboard`.
Check status at `http://localhost:8080/status`.

For a step-by-step walkthrough including issuing your first API key and making your first request, see [docs/quickstart.md](docs/quickstart.md).

## Connect your app (1 line change)

### Python — change only the `base_url`

```python
# Before
client = OpenAI(api_key="sk-...")

# After
client = OpenAI(
    base_url="http://localhost:8080/v1/proxy/openai/v1",
    api_key="tlv_your_lens_key",
)
```

### Python — using the native SDK (3 lines)

```python
from talyvor_lens import LensClient
client = LensClient(lens_url="http://localhost:8080", api_key="tlv_...")
response = client.openai.chat.completions.create(model="gpt-4o", messages=[...])
```

See [`sdk/python/README.md`](sdk/python/README.md) and [`sdk/typescript/README.md`](sdk/typescript/README.md).

### Other providers

| Provider | URL path |
|---|---|
| OpenAI | `/v1/proxy/openai/v1/chat/completions` |
| Anthropic | `/v1/proxy/anthropic/v1/messages` |
| Google Gemini | `/v1/proxy/google/*` |
| AWS Bedrock | `/v1/proxy/bedrock/*` |
| Mistral | `/v1/proxy/mistral/chat/completions` |
| Groq | `/v1/proxy/groq/chat/completions` |
| vLLM | `/v1/proxy/vllm/chat/completions` |
| Helicone-compat | `/oai/v1/chat/completions`, `/anthropic/v1/messages` |

## Dashboard

`http://localhost:8080/dashboard` surfaces:

- Real-time spend by model + workspace
- Cache hit rate (exact + semantic)
- Top cached prompt patterns
- Circuit-breaker status per team/feature
- Local model availability
- Workspace logging policy (full / metadata / none)
- Live cost anomalies (`>3σ` z-score)

## Migrating from another gateway

- **From Helicone** — see [docs/migrate-from-helicone.md](docs/migrate-from-helicone.md). One-line URL change; the `Helicone-Auth` and `Helicone-Property-*` headers keep working through the compatibility layer.
- **From LiteLLM** — see [docs/migrate-from-litellm.md](docs/migrate-from-litellm.md). One `base_url` flip; no Python supply-chain risk.

## SDKs

- Python: `pip install talyvor-lens` — [README](sdk/python/README.md)
- TypeScript: `npm install talyvor-lens` — [README](sdk/typescript/README.md)

## Operations

- Status page: `GET /status` (HTML) or `/status.json`.
- Health probe: `GET /healthz`.
- Audit export: `GET /v1/audit/export?format=json|csv|ndjson` (streams).
- Anomaly scan: `GET /v1/api/anomalies/scan`.
- Run benchmarks: `make bench`.

## Documentation

Full index at [`docs/README.md`](docs/README.md). Highlights:

- [Quickstart](docs/quickstart.md)
- [Migration guides](docs/README.md#migration-guides)
- [Benchmarks](benchmarks/README.md)

## Architecture

Single Go binary, no Python or Node runtime. PostgreSQL (with pgvector) for state, Redis for the hot exact cache + rate-limit ledger, NATS for the learner / anomaly event bus.

Test coverage: 37 Go packages, all green. Python SDK: 15/15. TypeScript SDK: 15/15.

## License

Open core. The proxy and every package under `internal/` is MIT-licensed. SSO, compliance exports, and SLA support are available commercially — contact `hello@talyvor.com`.
