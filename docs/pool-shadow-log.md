# The pooled-cache shadow log — how it is recorded and how it is read back

W4.9 asks for a log of what **would** have pooled, so the cross-tenant pooled hit rate becomes
measurable on real traffic while pooling itself stays **off** for consumers (W2.1 measured danger
pairs above the production threshold; W2.6 measured the canonicaliser serving one rephrasing in 68).

## The design constraint that shaped it

The obvious shadow log is: *on a private miss, do the pooled lookup anyway and record the would-be
hit without serving it.* **It can only ever record zero.**

Both pooled read surfaces — `proxy.tryExactPooled` and `proxy.trySemanticPooled` — read a keyspace
that is written **only** under `poolGate.DecidePoolableOnWrite`, and read and write both route
through `cache_pooling.PoolabilityGate.Participant` (global switch AND workspace opt-in). With
pooling off the pooled keyspace is **empty**, so the lookup misses every time, forever. The number
would be structurally zero and would read as measured — and the next reader would conclude the pool
has no value.

So the log is written from the **write side** and the hit rate is computed **offline from repeats**.
Nothing reads the pooled keyspace at any point. `TestShadowPool_DoesNotReadThePooledKeyspace` pins
that: it fails if this file's hook ever calls one of the read surfaces.

## How it is recorded

One row per **fresh, cacheable, non-PII response that came back from a paid provider** — the exact
population whose repeats would have been pool hits. Written post-serve, void, error-swallowed; it
cannot block, delay, fail or alter a request. Off unless `LENS_POOL_SHADOW_LOG_ENABLED=true`.

Table `pooled_shadow_observations` (migration `0125`) stores **fingerprints only** — no prompt text,
no response bytes, no cost, no token counts, no ledger units:

| column | what it is |
|---|---|
| `pooled_key_fp` | SHA-256 over the **production** pooled key material (`cache.PooledPromptKey`), namespaced by provider+model |
| `canon_fp` | SHA-256 over `discriminator.Canon(prompt)`; NULL when the canonical form is empty |
| `workspace_id`, `provider`, `model`, `observed_at` | attribution and time |
| `did_pool` | whether the pooled copy was **actually** written (the gate was on) |

The pooled key is passed in from the proxy rather than re-derived, so production owns the key rule
and the shadow numbers cannot drift away from the pool they describe.

## How it is read back

`poolshadow.Recorder.Rate(ctx, since, ttl)`, or the SQL in the migration header. It returns three
figures **from one pass**, so they always describe the same population:

- **`Observations`** — the denominator. A rate quoted without it is a rate over an unknown population.
- **`CrossTenantExactHits`** — observations with an earlier observation of the same `pooled_key_fp`,
  from a **different** workspace, **within the pooled entry's TTL**. All three conditions are
  load-bearing: a same-workspace repeat is a *private* cache hit and pooling buys nothing there; the
  first sighting is the contribution, not a hit; and an entry that has aged out could not have been
  served. `ttl` is an **argument** — the caller passes `cfg.MaxCacheTTL`, so this code never names a
  duration nobody measured.
- **`CrossTenantEntityGateCeiling`** — an **upper bound on the semantic lane, never a hit count.**
  `cmd/hitrate` measured that `semanticSelectPooledSQL`'s `discriminators = $6` exact entity equality,
  not the cosine threshold, is what bounds the semantic pool, and that this bound is
  threshold-independent. A cross-tenant repeat of `canon_fp` is a pair the entity gate *would* have
  admitted — necessary for a semantic serve, never sufficient. Similarity is not recorded here and
  cannot be inferred from here. **Anyone quoting this as a hit rate is quoting a ceiling as a
  measurement.**

## What the number is NOT — three ways it is a floor

Stated here rather than discovered by whoever quotes it:

1. **`LoggingNone` traffic is excluded.** `storeCaches` deliberately does *not* consult the logging
   policy — that is the open decision `retention-none-and-the-semantic-cache.md` records and
   `logging_none_cache_test.go` pins — so a `LoggingNone` workspace's response **is** cached and
   would be pooled, while this observation is skipped. Writing a metadata row for a tenant that asked
   for no rows would not be acceptable, so the log under-counts by that share of traffic instead.
2. **Locally-served and node-served lanes are excluded.** `tryLocalRouting` and `tryNodeRouting`
   write the cache without a paid cloud call, so a pooled hit on one avoids no provider spend — but
   `storeCaches` *does* put their responses in the pooled keyspace, so in production a cloud request
   could be served from one. Those hits are invisible here.
   `TestShadowPool_CoveragePopulationIsExplicit` requires any future uncovered lane to be named with
   a reason rather than merely absent.
3. **The semantic lane is a ceiling, not a count** (above).

Both paid-provider lanes — buffered (`proxy.go serve`) and streaming (`stream.go serve`) — **are**
covered. That census keys on **file + function**, because both lanes are called `serve`: keyed on the
name alone it could not fail for the streaming lane, which is how it was first written.
