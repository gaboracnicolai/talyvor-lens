# Cross-tenant pooling as it runs in production — measured 2026-09-25 (B9.1)

> **Since measured:**
> - B9.8 (#544) charges every browser-chat request.
> - B9.3 charges a chat **pooled** serve like an agent-key one: list × 0.70 from the allowance or prepaid, with the contributor's held royalty at half the charge. Tested in `TestPoolB93_ChatPooledServe_ChargedDiscountedAndRoyaltyPaid`.
> - Every `token_events` row now records its `auth_method`.
>
> The chat rows below describe the code as it was on 2026-09-25.

Re-run everything below with `scripts/measure-pool.sh <ssh-target>`. It only reads production: a
read-only Postgres transaction, the running container's environment, and a one-off container
that scores the corpora and never touches the database. No threshold or gate was changed.

## The answers

| Question | Measured |
|---|---|
| Is pooling on in production? | **Yes.** `POOLING ENABLED: the live embedding configuration matches the last passing poolcheck` (text-embedding-3-small, threshold 0.98) |
| Wrong answers the corpora would get served at production's settings | **0.** Engineering 0/44, consumer 0/42 |
| Rephrasings the corpora would hit | **2 of 68.** Engineering 2/30, consumer 0/38 |
| Pooled serves in production, all time | **6**, all exact-match, 2026-07-27 to 07-29. **0 semantic** |
| Pool contents now | **Empty.** 0 exact entries in Redis, 0 rows in `prompt_embeddings`. There has been no traffic since 2026-08-19 |
| Identical prompt from a second workspace, agent key | **Served from the pool and billed correctly.** The asker pays list minus 30%, and the contributor is credited half of what the asker paid |
| Identical prompt from a second workspace, browser chat (session key) | **Served from the pool, charged $0, royalty 0.** Chat's pooled serves earn nothing and pay the contributor nothing |
| The chat's "would have pooled" log | **No chat-specific log exists.** The general one (`pooled_shadow_observations`) has **0 rows**, and its switch cannot be turned on under compose (see below) |

## What serves cross-tenant today, and behind which gates

Both lanes run only after the asker's own exact and own semantic cache both miss. They sit on the
same code path for buffered and streamed requests: a streamed request that hits is replayed as
SSE (`proxy.go:1155`).

**Exact lane.** The key is the byte-identical prompt (`model` plus `messages[].content`) under the
pool marker, for the same provider and model. The lane has no similarity threshold and no entity
gate. Entries live in Redis for `LENS_MAX_CACHE_TTL` (unset, so 24h).

**Semantic lane.** A pooled row is served when all of these hold:
- cosine ≥ **0.98** (`LENS_SEMANTIC_THRESHOLD` is unset, so the default applies);
- discriminators are exactly equal (the entity gate), and the prompt has at least one entity;
- the provider, model and embedding model are the same;
- the row was updated within `LENS_SEMANTIC_CACHE_RETENTION` (unset, so 21 days).

Rows are written only for prompts with entities.

**Gates both lanes pass:**

| Gate | Production value |
|---|---|
| Global switch | `LENS_CACHE_POOLABLE_ENABLED=true` AND `LENS_ECONOMY_ENABLED=true` AND the attestation matches the live config |
| Attestation (`pool_safety_attestation`) | text-embedding-3-small, **0.92**, worst pair "review preamble · tiny diffs" 0.653, checked 2026-07-28. Live must use the same model and a threshold ≥ 0.92 |
| Asker and contributor opted in (`workspaces.cache_poolable`) | **8 of 8** workspaces, so 56 ordered cross-tenant pairs are eligible |
| Not PII, chat endpoint only | per request |

**Money on a pooled serve:**
- The asker is charged list × (1 − 0.30) (`LENS_POOL_CONSUMER_DISCOUNT` is unset). This charge happens **only when the request settles an agent-key reservation**.
- The contributor is minted `LENS_POOL_ROYALTY_SHARE` = 0.5 of that charge, held for 72h, then final. Minting is on.
- **A serve with no charge mints nothing, by design** (the funding invariant).

## Production counts

| | |
|---|---|
| Workspaces / opted in | 8 / 8 |
| `token_events`, all time | 27: upstream 17 · own exact 2 · own semantic 2 · **pooled exact 6** · pooled semantic 0 |
| Pooled serves: asker workspaces | 3. `token_events` has no contributor column; the contributor is recorded only on a royalty claim |
| Pooled serves charged to the asker | **2 of 6**, each 1,645 µLXC against a list price of 2,350 (705 saved, 30%). The other 4 were served free, with no agent reservation |
| Royalty claims | **1**, 822 µLENS: held 07-29, final 08-01. 1 of the 2 charged serves minted. The other's refusal reason is written only to the log, and those logs have rotated |
| Pool entries now | Redis `lens:exact:*` **0** · `prompt_embeddings` poolable **0**, private 0 |
| "Would have pooled" rows | **0** |

**Why the would-have-pooled log is empty.** `pooled_shadow_observations` (migration 0125) is written
only when `LENS_POOL_SHADOW_LOG_ENABLED=true`. That variable is read in `config.go` but is **not in
`docker-compose.yaml`'s lens `environment:` list**, so under compose it can never be turned on.
This is logged in FOUND.md; the item changes no switch.

## The committed corpora at production's configuration

The corpora are `poolsafety.ByTraffic()`, 154 pairs. They were scored by `cmd/hitrate` inside
production's lens container environment, with production's embedding key and model, on
2026-09-25.

The columns:
- **production** is cosine ≥ t AND equal discriminators, which is what the SQL checks.
- **as-written** is new in this merge. It is the same check with the stored side's discriminators computed the way the serve path actually writes them (see the next section).

| lane | threshold | rephrasings hit (production / as-written) | danger pairs served (production / as-written) | danger by similarity alone |
|---|---|---|---|---|
| engineering | **0.98 (live)** | **2/30 · 2/30** | **0/44 · 0/44** | 1/44 (`cache-ttl-ms` 0.9829, stopped by the entity gate) |
| engineering | 0.92 (attested) | 2/30 · 2/30 | 0/44 · 0/44 | 7/44 |
| consumer | **0.98 (live)** | **0/38 · 0/38** | **0/42 · 0/42** | 0/42 |
| consumer | 0.92 (attested) | 0/38 · 0/38 | **1/42 · 1/42** (`isa-year` 0.9377, entities equal) | 3/42 |

The attestation admits any live threshold ≥ 0.92. At 0.92, the consumer lane **would serve a wrong
answer** (`isa-year`). Production is safe today only because the threshold is 0.98. Nothing was
changed.

The entity gate caps what any threshold can reach: at most 14/30 engineering rephrasings and 0/38
consumer rephrasings. Consumer questions mostly carry no entity at all (in 30 of 38 rephrasings, neither side has one), so
**the semantic lane cannot serve consumer traffic at any threshold**.

## The semantic write and read disagree for some prompts (measured, not fixed)

`storeCaches` hands `SetPooled` the marker-prefixed key, so a pooled row's discriminators are
`Canon("\x00pool\x00" + prompt)`. `GetPooled` compares `Canon(prompt)`. The sentence-start rule is
anchored on `^`, so on write only, a first word that is capitalised and not a stopword becomes a
proper noun:

    "Can I enable SSO for Okta?"   write: caps:sso|propn:can|propn:okta   read: caps:sso|propn:okta

A pooled row whose prompt opens that way can never be served semantically. On these corpora the
as-written column matches the production column at every threshold, so it changes none of the
figures above. It only makes serving rarer and never serves a wrong answer. It is logged in
FOUND.md.

## An identical question from a second workspace, end to end

The test is `internal/proxy/pool_b91_endtoend_realpg_test.go`. It uses real Postgres, the real
proxy handler and the real minter, at production's discount (0.30) and share (0.5). Workspace A
asks and pays list price. Workspace B then sends the byte-identical prompt.

- **Agent key:**
  - The upstream is called once. B's `token_events` row is `serve_source = cache_hit_pooled`.
  - B's `lxc_ledger` row charges **1,519 µLXC**: list 2,170, saved 651, rate 0.30, and charged + saved = list.
  - `pool_royalty_mints` holds one claim (requester B, contributor A, layer `exact`), with `avoided_cogs_usd` equal to B's charge in USD and `minted_amount` = **759 µLENS**, which is ⌊1,519 / 2⌋.
  - `lens_token_ledger` holds one `pool_royalty_held` row for A of exactly 759.
  - Production's single royalty has the same shape: 1,645 charged, 822 minted.
- **Browser chat, session key:** the serve comes from the pool (the upstream is still called only
  once), but there are **0 charge rows and 0 royalty claims**. The log reads "consumer paid $0 …
  royalty UNFUNDED, not minted". Four of production's six pooled serves were likewise uncharged. Their auth method is not recorded, so whether they came from chat or a plain key is unknown.
