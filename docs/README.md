# Talyvor Lens Documentation

Index of every doc shipped with the repo. The main [project README](../README.md) lives at the repo root.

## Migration guides

- [Migrate from Helicone](migrate-from-helicone.md) — 1-line change. Helicone wire format (URL + headers) accepted as-is via the compatibility middleware.
- [Migrate from LiteLLM](migrate-from-litellm.md) — `base_url` flip + new API key. Drops Python supply-chain risk and ~8 GB of memory.

## SDKs

- [Python SDK](../sdk/python/README.md) — 3-line install, drop-in `openai` / `anthropic` wrapper.
- [TypeScript SDK](../sdk/typescript/README.md) — 3-line install, drop-in `openai` wrapper.

## Operations

- [Local standup runbook](local-standup-runbook.md) — bring Lens up standalone and take it from zero to a first real served request that mints a `lens_token_ledger` row; documents every silent-zero trap (stale image, both provider keys, the two pattern flags, `earn_verified`, the LXC bootstrap grant).
- [Benchmarks](../benchmarks/README.md) — performance suite + reproducible numbers vs LiteLLM / Portkey.
- [Status page](../README.md#status) — public health surface at `/status` and `/status.json`.

## API surface

The full API is documented inline in `internal/api/server.go` (`MountAuthenticated`). High-traffic endpoints:

- `POST /v1/proxy/{openai,anthropic,google,bedrock}/*` — provider proxies.
- `POST /oai/*`, `POST /anthropic/*` — Helicone-compatible URL prefixes.
- `GET  /v1/api/spend/summary` — workspace spend rollup.
- `GET  /v1/audit/export?format=…` — streaming audit log.
- `GET  /v1/api/anomalies/scan` — current cost anomalies.
- `POST /v1/guardrails/check` — pre-flight prompt scan.
- `POST /v1/eval/run` — synchronous eval suite run.
- `GET  /v1/api/keys/pool` — API key pool stats.
- `POST /mcp` — JSON-RPC 2.0 endpoint for agent frameworks.

## Schema migrations

`migrations/00NN_*.sql` files apply in numeric order. New migrations are idempotent (`IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS`) so reapplication is safe for blue-green deploys.

## Contributing

- `make test` runs the full Go suite.
- `make vet` runs `go vet ./...`.
- `make bench` runs the performance suite (gated behind `-tags=bench`).
- Python SDK: `cd sdk/python && pytest tests/`.
- TypeScript SDK: `cd sdk/typescript && npm test`.
