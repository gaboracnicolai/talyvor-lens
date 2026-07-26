# Talyvor Lens

**AI token intelligence proxy — sit between your app and the providers you already use, and pay for fewer provider calls.**

Lens cuts spend by not making calls: an exact and semantic response cache, cross-tenant
reuse of cached answers, cross-provider routing, and document distillation. **We do not
publish a headline savings percentage, because we have not finished measuring one** — see
[What we can and cannot tell you about savings](#what-we-can-and-cannot-tell-you-about-savings).

Drop-in replacement for OpenAI, Anthropic, Google Gemini, AWS Bedrock, Mistral, Groq, and vLLM. Change one URL. Get caching, routing, attribution, guardrails, audit, and fallback.

Lens is an API. The dashboard is a separate app ([app.talyvor.com](https://app.talyvor.com), or self-host `talyvor-suite`); the Lens host itself serves only a small service page at `/` and component health at `/status`.

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

Comparison from public benchmarks and vendor docs. `make bench` reproduces the *latency and
allocation* figures on this page (it benchmarks the proxy against a mocked upstream); it does
not measure cost.

## What we can and cannot tell you about savings

Every gateway in this category advertises a savings range. Ours used to say 60–80%. Nothing in
this repository computes that number, so it is gone rather than restated.

**The mechanisms are real and you can inspect each one:**

| Mechanism | What it avoids | Where |
|---|---|---|
| Exact cache (Redis) | The whole call, on a repeat request | `internal/cache` |
| Semantic cache (pgvector) | The whole call, on a near-duplicate | `internal/cache/semantic.go` |
| Cross-tenant pooled reuse | The whole call, using another workspace's cached answer (opt-in, both sides) | `internal/cache_pooling` |
| Cross-provider routing | The price difference between a model you asked for and a cheaper one that measured equal on your traffic (opt-in per workspace) | `internal/routing` |
| Document distillation | Re-sending the same document as raw tokens | `internal/distill` |

**What we will not do is multiply a cache-hit rate by your bill and call it a saving.** How much
you actually save depends on how repetitive your traffic is and how well you were already
choosing models — and for a workload that is already tightly specified, the honest answer for
some of these mechanisms is close to zero.

### The baseline has to be an already-optimised one

This is the part most savings claims get wrong, including ours. **Providers now discount cached
input by roughly 90%** (Anthropic bills a cache read at 0.1× input; OpenAI at roughly 0.5× for
the GPT-4o generation). That discount is free — you get it whether or not Lens is in the path.

A percentage measured against naive, full-price, no-caching spend therefore **counts a discount
you already had as if we produced it**. Any figure we eventually publish will be measured against
a baseline that already includes provider-side caching.

Lens's own cost basis already works this way: `alerts.CostUSDDetailed` prices cached-input and
cache-write tokens at the provider's real multipliers (`catalog.withCacheRates`), and the rates
are deliberately set on the conservative side — where a provider's discount is steeper than we
model, we under-state it, so a savings figure derived from this basis errs low.

### Measuring it on your own traffic

The routing path records a per-request counterfactual: what the call cost, and what the model you
originally asked for would have cost. Admins can read the aggregate:

```
GET /v1/admin/routing-decisions/summary
```

It returns request volume, how often routing overrode your model choice, and the estimated cost
delta. **It is an estimate, not money** — the counterfactual call never happened, so its price is
modelled, not billed. Treat it as the shape of the effect on your traffic, not as an invoice.

A measured, published figure — against an already-optimised baseline, on real customer workloads
— is intended. It is not in this README because it does not exist yet, and the point of this
section is that a number nobody computed should not be the first thing you read.

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

Open `http://localhost:8080/` for the service page (what this host is, live health,
where to go next), or `http://localhost:8080/status` for per-component health.

The dashboard is a separate app — [app.talyvor.com](https://app.talyvor.com), or
self-host [`talyvor-suite`](https://github.com/gaboracnicolai/talyvor-suite). Lens
itself is an API and serves no account UI.

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

## Seeing your data

Lens is an API: it serves no account UI of its own. These reads are all authenticated
API endpoints — point the dashboard app at them, or call them directly:

| | |
|---|---|
| Spend by model / workspace | `/v1/api/spend/summary`, `/v1/api/spend/by-model` |
| Cache hit rate + top patterns | `/v1/api/cache/stats`, `/v1/api/cache/top-patterns` |
| Per-model usage + serve source | `/v1/api/usage` |
| Circuit-breaker status | `/v1/api/alerts/circuits` |
| Local model availability | `/v1/api/local/status` |
| Live cost anomalies (`>3σ`) | `/v1/api/anomalies/scan` |

The dashboard is [app.talyvor.com](https://app.talyvor.com) (or self-hosted
[`talyvor-suite`](https://github.com/gaboracnicolai/talyvor-suite)), which holds your
key server-side and calls these for you. Unauthenticated, this host serves only `/`
(service page) and `/status` (component health).

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

## What is on by default

A self-hoster's most reasonable question, and one this README previously answered wrongly.

| Switch | Default | Effect |
|---|---|---|
| `LENS_ECONOMY_ENABLED` | **true** | Master economy switch. Registers the economy route surface and permits mint/earn/stake/marketplace state. Set `false` for a pure fiat-SaaS deployment — it then force-offs every economy gate regardless of their own env values. |
| Pool-royalty / distill / pattern mints | **on** (under the master switch) | The three live traffic mints, default-on for the closed test. |
| `LENS_ANNOTATION_MINTING_ENABLED` | false | Annotation mint — spendable-immediate, so explicit opt-in. |
| `LENS_TRUSTFUL_COMPUTE_MINT_ENABLED` | false | Legacy receipt-less compute mint. An unprotected mint path is opt-in, never on by accident. |
| `LENS_MINT_RATE_CAP_LENS_24H` | 1000 | Per-workspace ceiling on minted LENS across **all** mint types in a rolling 24h. `0` disables. |
| `LENS_BILLING_ENABLED` | false | Stripe billing / LXC purchase. Off means no one can buy LXC on this deployment. |

**The important consequence:** economy-on does *not* mean a fresh workspace earns anything. Every
mint-type credit passes the **verified-to-earn** gate below, which requires a completed real-money
LXC purchase or an admin vouch. A default deployment has the economy *armed* and mints *nothing*
until a workspace is verified. That is a meaningfully different statement from "off by default",
and the previous wording obscured it.

## Architecture

Single Go binary, no Python or Node runtime. PostgreSQL (with pgvector) for state, Redis for the hot exact cache + rate-limit ledger, NATS for the learner / anomaly event bus.

Test coverage: 88 of 92 Go packages carry tests, all green in CI (real-Postgres, `-race`). The
count in this line was stale at 37 for some time — it is now derived from the tree, so treat it
as accurate at the commit you are reading and re-derive with
`find . -name '*_test.go' | sed 's|/[^/]*$||' | sort -u | wc -l` if it matters to you.

## LENS Token Economy

LENS is a compute-backed utility token. You earn it by contributing infrastructure to the
network. There are **two units** and they behave differently — the distinction matters before
any number below makes sense:

- **LXC** is the billing credit, and its peg is **fixed**: 1 LXC = $0.10 of compute credit,
  never computed or adjusted (`economy.LXCUSDValue`). This is what inference is billed against.
- **LENS** is the mined token. `1 LENS = $0.10` is its **published nominal** peg
  (`marketplace.LENSPerUSD`), but LENS does not convert to LXC at a fixed rate: the conversion
  runs through a **floating** rate engine (`economy.RateEngine.ComputeFairRate`) derived from
  supply and backing, plus a 5% spread, bounded to ±10% movement per approval and floored until
  a live marketplace price exists. **Treat $0.10/LENS as a unit of account, not a redemption
  guarantee.**

Everything in this section is gated on the economy master switch (see
[What is on by default](#what-is-on-by-default)), and staking or trading requires a workspace
that already holds LENS.

### Mining Types

| # | Track | What you contribute | Earn rate |
|---|---|---|---|
| 1 | **Pool royalty** | A cached answer of yours reused by *another* workspace (opt-in both sides) | `s` × the provider cost that call avoided; `s` = 0.5 by default (`LENS_POOL_ROYALTY_SHARE`) |
| 2 | **Compute mining** | GPU inference capacity (Ollama / vLLM / llama.cpp) | 0.025–0.150 LENS / 1k tokens (by GPU class) |
| 3 | **Embedding mining** | CPU-friendly embedding generation | 0.002–0.004 LENS / 1k embeddings |
| 4 | **Quality oracle** | Stake-gated annotation of LLM responses | 0.100 LENS / annotation + agreement bonus |
| 5 | **Pattern mining** | Anonymised routing patterns (opt-in) | 0.001 LENS × (1 + rarity × 4), but see the reachable ceiling below |

**Row 1 changed, and the old row was wrong.** This table used to list a *cache-mining* track at
"0.001–0.010 LENS / hit". That track (`CacheMiner`) was **retired**, not renamed: it duplicated
pool-royalty at the same serve point (and would have double-minted if both were wired), and its
one unique path — minting for a hit on your *own* cache — is self-inflation, since no second
party received anything. The cache moat is real; it is delivered by pool-royalty, which pays only
when the value actually goes to someone else. The `talyvor-cachenode` binary is unaffected: you
still contribute cache capacity with it, you now earn through row 1.

**Pattern mining's ceiling is 2×, not 5×.** The formula reads as though rarity 1.0 pays a 5×
multiplier. It cannot: the anti-gaming corroboration floor caps reachable rarity at 0.25, so the
reachable ceiling is `1 + 0.25×4 = 2.0`. The public `/v1/tokens/rates` API was corrected to
advertise the reachable 2× some time ago; this table was not, until now.

### Node Software

| Binary | Default port | Purpose |
|---|---|---|
| `talyvor-lens` | 8080 | The Lens proxy itself |
| `talyvor-node` | 9090 | GPU inference mining |
| `talyvor-cachenode` | 9091 | Cache contribution mining |
| `talyvor-embednode` | 9092 | Embedding farm mining |

Build all four: `make binaries` (drops them into `./bin/`).

### Token Economics — built and wired

Each of these is implemented, wired to a route, and gated on the economy master switch. All of
them additionally need a workspace that already holds LENS.

- **Staking**: 5% / 12% / 20% APY for 30 / 90 / 180-day locks. Rates are hardcoded
  (`economy.APY30/APY90/APY180`) so a misconfigured deployment cannot pay an arbitrary yield;
  read via `/v1/workspaces/{ws}/tokens/stakes`.
- **Marketplace**: peer-to-peer LENS trading with a **5% platform fee**
  (`economy.TalyvorFeeRate = 0.05`) — seller receives 95%. Listings at
  `/v1/marketplace/listings`.
- **Quality oracle stake**: **10 LENS** minimum lockup before an annotation is accepted
  (`mining.StakeRequirement`), Sybil-resistant.
- **LXC peg**: 1 LXC = $0.10, fixed (see above).

### Token Economics — roadmap, not built

**These are deliberate future work for the agent-service marketplace**, where agents transact in
LENS with each other directly rather than a human paying a fiat invoice. They are listed here so
they are not mistaken for shipped features — and so nobody deletes them later as dead weight,
because the burn primitive is a real, tested part of the eventual design.

- **LENS-burn discount — NOT BUILT.** The intent is that spending LENS on inference costs less
  than paying fiat, with the burned LENS leaving circulation permanently. **No discount
  multiplier exists anywhere in the codebase today**; the only trace is a comment in
  `economy/marketplace.go` naming the future path. Paying with LENS currently gets you no
  discount, because there is no LENS-payment path for inference at all.
- **Burn in circulation — PRIMITIVE ONLY.** `mining.LedgerStore.Burn` is implemented and tested,
  and `GetTotalBurned` is wired into the economy-stats readout — but **it has no production
  caller**. Nothing in a running Lens ever burns LENS, so total-burned reads zero and will keep
  reading zero until the discount path above exists. The primitive is kept deliberately: it is
  the supply-side half of the burn-and-mint design the marketplace needs.

Why they are not simply deleted: the agent marketplace is the reason the token is a token rather
than a loyalty-points balance. Removing the burn machinery would mean rebuilding it, and the
tested primitive is the cheap half.

### Sybil resistance (verified-to-earn)

**Correction:** this section used to say the token economy "ships dark (off by default)". It does
not. `LENS_ECONOMY_ENABLED` **defaults TRUE** (`config.go`: `c.EconomyEnabled = true`, explicit
opt-out), and three traffic mints — pool-royalty, distill and pattern — are default-on under it.
What actually keeps a fresh deployment from minting value is not the master switch but the **U6
Sybil floor** below, which is wired unconditionally and which the master switch cannot lift:

- **Verified-to-earn gate.** A workspace may mint / accrue royalty only when it is **verified-to-earn**: it has a **completed real-money LXC purchase** (derived at read time) OR an admin-set `earn_verified` flag (the enterprise / self-host vouch). Refunded / anomalous purchases do **not** count (closes the buy→refund→stay-verified loop). The gate is enforced at the **ledger chokepoint** (`applyTx` + `heldInner`): every mint-type credit — cache, compute, embedding, annotation, pattern, PoVI receipt, and the pool-royalty held mint — passes through it; conservation moves (marketplace, unstake, LENS→LXC convert) are never gated. The gate is wired **unconditionally** — a safety restriction the economy master-switch cannot lift.
- **Idempotent mints.** The previously-unprotected compute / cache / embedding tracks now claim a `(request_id, workspace_id, mint_type)` row before crediting (the pattern track's proven shape). `request_id` must be **server-derived** work-product content; an empty id mints nothing.
- **Legacy trust-mint off by default.** The receipt-less compute mint (`LENS_TRUSTFUL_COMPUTE_MINT_ENABLED`) now **defaults false** — an unprotected mint path is opt-in, not on-by-accident.

The **PR2 wash-hardening** then bounds the steady-state yield (verification raised the *entry* bar but not the *steady-state* yield, which a determined operator amortizes):

- **Per-identity rate cap (the universal bound).** A per-workspace rolling **24h** ceiling on **minted LENS across all mint types**, enforced at the same ledger chokepoint (`LENS_MINT_RATE_CAP_LENS_24H`, default **1000** LENS/24h, `0` = off). It sums every mint type together — an attacker can't evade by splitting across tracks — and is exact under concurrency (the SUM rides the balance `FOR UPDATE`). Held mints count at the mint moment; the finalize settlement is **not** double-counted. Conservation moves are never throttled.
- **Card-fingerprint owner-linkage (the cheap bonus).** The Stripe webhook captures a **hash** of the card fingerprint (never the raw value) **best-effort, after the credit commits** — a capture failure can never drop the payment. A pool-royalty mint between two workspaces that share a fingerprint (one operator, one card) is denied; **default-allow on missing** (an absent fingerprint never blocks honest cross-actor reuse). Catches the lazy one-card washer.

**Residual (honest):** a determined operator can still wash **under the rate cap** across **many cards** (rotating cards evades the fingerprint linkage). The rate cap bounds the per-identity yield; deeper owner-linkage (e.g. network/behavioral signals) carries a high privacy cost and is deferred. The verification cost + the rate cap + the cheap linkage together make casual washing unprofitable and bound the determined case.

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

### Reading the economy

There are no built-in browser pages for these; the reads are API endpoints:

- `/v1/workspaces/{ws}/tokens/balance`, `.../tokens/mining/*` — balance and mining (authenticated)
- `/v1/workspaces/{ws}/tokens/stakes`, `/v1/marketplace/listings` — staking and listings
- `/v1/economy/stats`, `/v1/tokens/rates`, `/v1/oracle/stats` — global supply, rates and the
  oracle queue (public, present only when the economy is enabled)

## License

[Business Source License 1.1](LICENSE) (BUSL-1.1). **Not an open-source licence today.**

You may read, modify and self-host Talyvor Lens, including in production, for your own
organisation's purposes without limit, and an integrator may run it for up to **three clients
at a time**, each on its own deployment. You may **not** run one deployment serving two or more
unrelated organisations. Beyond three concurrent client engagements, or for multi-tenant use,
that is a commercial licence rather than a refusal — `hello@talyvor.com`. See the `Additional Use Grant` in [LICENSE](LICENSE) for the exact
boundary, and the `Change Date`, on which this converts to Apache License 2.0.

**The client SDKs are MIT, not BSL.** `sdk/typescript/` and `sdk/python/` are licensed under the
[MIT License](sdk/typescript/LICENSE) and are explicitly excluded from the Licensed Work above.
They are thin clients you embed in your own application and contain no Talyvor server logic, so
they should never put a licence review in the way of an integration.
