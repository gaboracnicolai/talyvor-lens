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

> **Image access:** `ghcr.io/gaboracnicolai/talyvor-lens` is a **private**
> package by decision — the binary embeds the full migration SQL (the
> pre-launch token-economy schema), so it is not published anonymously.
> Deploying hosts either authenticate once
> (`docker login ghcr.io -u <user>` with a PAT carrying `read:packages`)
> or build locally from a checkout (`docker compose build`, which the
> compose file supports out of the box).

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

## LENS Token Economy

LENS is a compute-backed utility token: **1 LENS = $0.10 of AI compute credit**. You earn LENS by contributing infrastructure to the network and spend it on LLM API calls through Talyvor Lens (with a 20% discount vs. paying fiat).

### Mining Types

| # | Track | What you contribute | Earn rate |
|---|---|---|---|
| 1 | **Cache mining** | Shared cache hits served to other workspaces | 0.001–0.010 LENS / hit |
| 2 | **Compute mining** | GPU inference capacity (Ollama / vLLM / llama.cpp) | 0.025–0.150 LENS / 1k tokens (by GPU class) |
| 3 | **Embedding mining** | CPU-friendly embedding generation | 0.002–0.004 LENS / 1k embeddings |
| 4 | **Quality oracle** | Stake-gated annotation of LLM responses | 0.100 LENS / annotation + agreement bonus |
| 5 | **Pattern mining** | Anonymised routing patterns (opt-in) | Rarity-weighted: 0.001 LENS × (1 + rarity × 4) [+ unique bonus] |

### Node Software

| Binary | Default port | Purpose |
|---|---|---|
| `talyvor-lens` | 8080 | The Lens proxy itself |
| `talyvor-node` | 9090 | GPU inference mining |
| `talyvor-cachenode` | 9091 | Cache contribution mining |
| `talyvor-embednode` | 9092 | Embedding farm mining |

Build all four: `make binaries` (drops them into `./bin/`).

### Token Economics

- **1 LENS = $0.10 USD** (compute-backed peg)
- **Burn mechanism**: spend LENS for a 20% discount on AI calls (burned LENS leaves circulation forever)
- **Staking**: 5% / 12% / 20% APY for 30 / 90 / 180-day locks
- **Marketplace**: peer-to-peer LENS trading with a 5% platform fee
- **Quality oracle stake**: 10 LENS minimum to annotate (Sybil-resistant)

### Sybil resistance (verified-to-earn)

The token economy ships **dark** (off by default). Before it can be flipped public, the **U6 Sybil floor** ensures value never mints to a free, duplicable identity:

- **Verified-to-earn gate.** A workspace may mint / accrue royalty only when it is **verified-to-earn**: it has a **completed real-money LXC purchase** (derived at read time) OR an admin-set `earn_verified` flag (the enterprise / self-host vouch). Refunded / anomalous purchases do **not** count (closes the buy→refund→stay-verified loop). The gate is enforced at the **ledger chokepoint** (`applyTx` + `heldInner`): every mint-type credit — cache, compute, embedding, annotation, pattern, PoVI receipt, and the pool-royalty held mint — passes through it; conservation moves (marketplace, unstake, LENS→LXC convert) are never gated. The gate is wired **unconditionally** — a safety restriction the economy master-switch cannot lift.
- **Idempotent mints.** The previously-unprotected compute / cache / embedding tracks now claim a `(request_id, workspace_id, mint_type)` row before crediting (the pattern track's proven shape). `request_id` must be **server-derived** work-product content; an empty id mints nothing.
- **Legacy trust-mint off by default.** The receipt-less compute mint (`LENS_TRUSTFUL_COMPUTE_MINT_ENABLED`) now **defaults false** — an unprotected mint path is opt-in, not on-by-accident.

**Residual (tracked for the fast-follow):** the floor raises the wash-trading bar from *two free workspaces* to *two verified identities*. Owner-linkage wash-hardening (so self-reuse between two identities the same operator controls doesn't pay) and a per-identity earning-rate cap are the next layer — they are **not** in this floor.

### Quick start (GPU miner)

```bash
export LENS_URL=https://lens.talyvor.com
export LENS_API_KEY=tlv_...
export LENS_WORKSPACE_ID=your-workspace
export NODE_URL=https://your-server.com
export NODE_PROVIDER=ollama
export NODE_MODELS=llama3.1,mistral
export NODE_GPU_TYPE=rtx4090
./bin/talyvor-node start
```

### Quick start (cache miner)

```bash
export LENS_URL=https://lens.talyvor.com
export LENS_API_KEY=tlv_...
export LENS_WORKSPACE_ID=your-workspace
export CACHE_NODE_URL=https://your-cache.example.com
export CACHE_NODE_REDIS_URL=redis://localhost:6379/0
export CACHE_NODE_MAX_GB=100
./bin/talyvor-cachenode start
```

### Quick start (embedding miner — CPU-friendly)

```bash
export LENS_URL=https://lens.talyvor.com
export LENS_API_KEY=tlv_...
export LENS_WORKSPACE_ID=your-workspace
export EMBED_NODE_URL=https://your-embed.example.com
export EMBED_NODE_MODEL=nomic-embed-text
export EMBED_NODE_DIMENSIONS=768
./bin/talyvor-embednode start
```

### Dashboard

The Lens dashboard at `/dashboard` includes per-workspace token views:

- `/dashboard/tokens` — balance, mining activity, staking, marketplace
- `/dashboard/nodes` — your registered inference + embedding nodes
- `/dashboard/oracle` — quality-oracle queue + annotation UI
- `/dashboard/economy` — global supply, circulation, listings

## License

Open core. The proxy and every package under `internal/` is MIT-licensed. SSO, compliance exports, and SLA support are available commercially — contact `hello@talyvor.com`.
