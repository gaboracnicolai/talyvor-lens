# Closed-test economy — fully ON (internal ledger, reversible)

The whole token economy, baked on in the committed trial compose for a **closed internal
test**: internal LENS ledger, valueless test tokens, Stripe **test mode** (no live keys),
not public, reversible (`docker compose … down -v`). Confirmed live end-to-end against a
running stack (see Part 3). Names/defaults verified against `internal/config/config.go` at
main `ac6dc82`.

## Bring it up
```sh
cp .env.production.example .env
echo "LENS_API_KEY=$(openssl rand -hex 32)" >> .env          # gitignored
docker compose -f docker-compose.yaml -f docker-compose.trial.yaml -f docker-compose.trial-distill.yaml up -d
```
(If a stale `pgdata` volume from a prior run causes a migrate `password authentication failed`,
`down -v` first — the password is set only on first DB init.)

## Flags ON (Part 1)

| Flag | Value | Default | Where | What it turns on |
|---|---|---|---|---|
| `LENS_ECONOMY_ENABLED` | *(unset → true)* | **true** (config.go:1094) | base default | Master switch. Left at default true; setting it false force-offs everything below (config.go:1098-1106). |
| `LENS_PATTERN_MINING_ENABLED` / `_CAPTURE_` / `_EARNING_` | true | false | trial.yaml | The pattern economy (S1–S4). |
| `LENS_CACHE_POOLABLE_ENABLED` | true | false | trial.yaml | Cross-tenant cache pooling (the Pool-B cache royalty substrate). |
| `LENS_DISTILL_POOLABLE_ENABLED` | true | false | trial-distill.yaml | Cross-tenant distill OCR pooling (the distill royalty substrate). |
| **`LENS_POOL_ROYALTY_MINTING_ENABLED`** | **true** | **false** (config.go:613) | **trial.yaml (new)** | The Pool-B **and** distill reuse-royalty mint. |
| **`LENS_POVI_MINTING_ENABLED`** | **true** | **false** (config.go:611) | **trial.yaml (new)** | The PoVI receipt mint (idle until a node submits receipts — Part 2). |
| **`LENS_POOL_HOLDBACK_WINDOW`** | **30s** | **72h** (config.go:845) | **trial.yaml (new)** | Held→final settlement delay. Staging-short so finalize is observable. |

**Billing stays TEST mode**: `LENS_BILLING_ENABLED` is left unset/false and **no live Stripe
keys** are set. The closed test uses the internal ledger + (optionally) the
`earn_verified`/test-`lxc_purchase` floor clear — no real money, and live Stripe is blocked by
the missing UK entity + bank regardless.

Boot confirmation (lens logs, observed):
```
WARN poolroyalty: Pool-B royalty minting is ENABLED … royalty_share=0.5
WARN PoVI receipt minting is ENABLED (LENS_POVI_MINTING_ENABLED) …
INFO audit retention sweeper disabled (LENS_AUDIT_RETENTION unset/<=0)   ← no pruning, footgun avoided
```

## Part 2 (validator-earning / PoVI): what's needed — NOT wired here

`LENS_POVI_MINTING_ENABLED=true` makes the gateway *ready* to mint from node receipts, but it
is **idle with no node submitting them**. Wiring `cmd/node` into compose so a node actually
earns end-to-end is **non-trivial** — it is a node-network integration, not a config tweak.
The precise blockers (all code-verified):

1. **Build `cmd/node`.** The Dockerfile (`Dockerfile:17,24`) builds only `./cmd/lens` +
   `./cmd/distill-worker`. A node binary/image + a `node` compose service must be added.
2. **A node-compatible inference provider.** `cmd/node` health-checks a local provider on
   start and **FATAL-exits if it's unreachable** (`cmd/node/main.go` runStart); the `vllm`
   provider probes `/v1/models` (`cmd/node/providers.go:229`). The trial `mockvllm`
   (`tools/trial/mock_vllm.py`) does **not** implement the node's `/v1/models` +
   `/v1/chat/completions` provider contract — so either extend it or add a real
   ollama/llama.cpp.
3. **Node identity + registration.** Env `LENS_URL`, `LENS_API_KEY`, `LENS_WORKSPACE_ID`,
   `NODE_URL` (the URL Lens reaches the node at — required for challenges), `NODE_MODELS`,
   `NODE_PROVIDER`, `NODE_PROVIDER_URL`. The node registers (`POST /v1/workspaces/{ws}/nodes`),
   gets a `NodeSecret`+`NodeID`, and signs receipts.
4. **Clear the node-owner workspace's U6 floor** (`earn_verified=true` or a completed
   `lxc_purchase>0`), else `MayEarn`=false → it mints **zero** (correct, not a bug).
5. **STAKE bootstrap — the keystone blocker.** The mint requires `stakeEligible =
   poviStakeManager.IsEligible(nodeID)` ≥ `LENS_POVI_MIN_STAKE` (default **100** LENS); a nil
   lookup defaults to `false` (`internal/povi/processor.go:41-42`). A fresh node has 0 LENS →
   can't stake → can't mint (chicken-and-egg). Pre-seed the node-owner workspace with ≥100
   LENS and stake it, **or** set `LENS_POVI_MIN_STAKE=0` for the test (if `IsEligible` permits
   a zero-stake node).
6. **Gateway→node traffic.** A node produces a receipt only when **it serves** an inference
   request; the gateway's `localRouterMulti` (synced via the control-plane `NodeSyncer`,
   `cmd/lens/main.go:586-600`) must route requests to the node. Cross-workspace / auto-route
   traffic must be generated so the node serves.
7. **A challenge-answerable node.** With minting on, challenge-and-slash runs
   (`LENS_POVI_CHALLENGE_RATE`); the node must pin Lens's challenge pubkey and answer
   Merkle-path challenges from its `TraceCache`, or it gets **slashed**. The trial currently
   generates an **ephemeral** challenge key (boot warns) — set `LENS_POVI_CHALLENGE_KEY`
   (base64 ed25519 seed) so a lens restart doesn't invalidate pinned node pubkeys.

Net: node-earning **code exists and is wired** (`cmd/node` daemon → `/v1/workspaces/{ws}/povi/receipts`
→ `poviProcessor.Process` → `MintFromReceipt`); what's missing is the **runtime substrate** (a
built node + a real provider + a staked, traffic-served, challenge-answering node), not new Go
logic. The legacy trust-mint path is separately dead (`NotifyServed` has no caller). Build this
as its own task — don't force a half-wired node (a half-wired node gets slashed).

## Part 3 — bring-up proof (distill royalty, end-to-end on the running stack)

Brought the stack up with all three overlays, seeded one `distill_royalty_basis` row
(`avoided_cogs_usd=2.0`) + cleared `wsA`'s floor (a completed test `lxc_purchase`), and let the
**live** distill mint + finalize sweepers run. Observed:

```
# the live mint sweeper (minute tick) → HELD
wsA | minted=1 | basis=2 | held          # minted_amount = 0.5 × 2.0 (s × pinned basis)

# after the 30s holdback → the finalize sweeper
status: final
bal|held: 1|0                            # held → spendable
supply row: 1 | pool_royalty            # the COUNTED TypePoolRoyalty ledger row (supply +1 at FINALIZE)

# scripts/verify-staging-economy.sh wsA wsB
PASS  (1) basis recorded         — 1 row(s), avoided_cogs_usd=2
PASS  (2) held mint == 0.5×basis — 1 mint(s) to wsA, minted_amount=1 (status=final)
PASS  (3) finalize → supply      — 1 finalized; counted 'pool_royalty' ledger sum for wsA = 1
RESULT: OK   (exit 0)
```

So the committed config, brought up, runs a live distill royalty mint on the internal ledger:
`s × avoided_cogs_usd` held → finalized to spendable → counted into supply at finalize. (The
PoVI line is **not** in this proof — Part 2 is not wired; the flag is on but idle.)

## Tear down (reversible)
```sh
docker compose -f docker-compose.yaml -f docker-compose.trial.yaml -f docker-compose.trial-distill.yaml down -v
```
