# `logging_policy = none` does not stop the semantic cache — measured

**Status: a MEASUREMENT and a DECISION REQUEST. Nothing in the commit that added this file changes
behaviour.** What it adds is `internal/proxy/logging_none_cache_test.go`, which turns the fact below
from a comment that is currently wrong into something CI re-measures on every run.

## The two claims

`internal/proxy/proxy.go`, beside the per-request observability writes:

> Logging policy gates the per-request observability writes. None is the privacy escape hatch —
> **every DB and NATS sink is bypassed.** Metadata keeps cost/token rows but strips `prompt_text`.
> Full keeps everything.

talyvor-suite `apps/web/src/areas/marketing/Landing.tsx` — the shipped marketing page:

> Prompts, issues, pages, and spend records sit in your Postgres. **Retention is a per-workspace
> policy you set — including "log nothing"** — not a plan tier.

## What was measured

`TestMeasured_LoggingNoneStillPersistsTheAnswerToTheSemanticCache`, against a real Postgres, using
the real `workspace.Manager`, the real `SemanticCache` and the real `Proxy.storeCaches`:

- the workspace is registered with `LoggingPolicy: none`, and the **Proxy itself** resolves it that
  way (`p.loggingPolicyFor(wsID) == none` is asserted before anything else happens);
- `storeCaches` is called once;
- the response body is then in `prompt_embeddings.response`, **verbatim**.

`prompt_embeddings` is the table this repo's own tenant-data manifest describes as *"the cached
ANSWERS — the most sensitive thing held"*.

## Why it happens

Three facts, each asserted by a census in the same file:

1. **`storeCaches` takes no logging policy and fetches none.** It cannot honour one.
2. **None of its four call sites** (`proxy.go` ×3, `stream.go` ×1) mentions the policy. They guard
   on HTTP status, PII detection and a quality score.
3. **`internal/cache` does not import `internal/workspace` at all**, so `SemanticCache.Set` has
   nothing to consult either. Its SQL writes `string(response)` unconditionally.

The policy ladder is enforced, carefully and with tests, for `token_events` — `none` skips the row,
`metadata` strips `prompt_text`, `full` keeps it. The semantic cache simply is not on that path.

## Scope — stated precisely, not dramatised

- The private semantic entry is **workspace-scoped**. It is not readable by another tenant; the
  cross-tenant *pooled* copy is a separate write behind an explicit `cache_poolable` opt-in.
- It is swept by `LENS_SEMANTIC_CACHE_RETENTION`, so it is not kept forever.
- The semantic cache is constructed **unconditionally** in `cmd/lens/main.go` — there is no feature
  flag — so this applies to any deployment with an embedding key configured.
- The exact (Redis) cache also holds the answer. This document is about the **Postgres** sink,
  because that is what "sit in your Postgres" and "log nothing" are about.

## The decision (NOT taken here)

Two readings are defensible and they lead to different products.

**(a) `none` must bypass the semantic-cache write too.** This is what the proxy's own comment and
the landing page say. ⚠ **It is not free**: a `none` workspace would lose every semantic cache hit,
so its requests go upstream and **its bill goes up**. That is a cost consequence for existing
customers, which is why a session must not merge it — this queue's rule about money paths.

**(b) The cache is operational, not observability, and is exempt.** Defensible: an entry has to hold
the answer to be a cache, it is workspace-private, and it is swept. ⚠ **Then two pieces of text are
wrong and must change**: the proxy comment claiming *every* DB sink is bypassed, and the marketing
page's "log nothing", which a customer reads as "no content of mine is retained".

**There is no third option where both the current behaviour and the current wording stand.**

A middle path exists — a separate cache-retention consent, orthogonal to `logging_policy`, the way
`cache_poolable` is orthogonal to it — but that is a new product concept and squarely a product
decision.

## What is merged

- `TestMeasured_LoggingNoneStillPersistsTheAnswerToTheSemanticCache` — the fact, executable.
  **Expected to go red when the behaviour is fixed**; its failure message says so and says to delete
  it then.
- `TestCensus_NoStoreCachesCallSiteConsultsTheLoggingPolicy` — asserts the absence, and **carries
  its own control**: the same search window, run over `recordTokenEvent` (a sink that *is* gated),
  must FIND that guard. Without it, "no guard found" would be indistinguishable from "this census
  cannot find guards".
- `TestCensus_StoreCachesItselfCannotHonourAPolicy` — the function neither receives a policy nor
  fetches one.

Controls: `w461-retention-controls-k7v3.py`, **4/4 CAUGHT**.

⚠ **Control F1 caught a flaw in this file's own evidence, and it is the part worth reading.** The
first fixture built a `Proxy` with no `workspaceManager` and registered the policy on a manager the
Proxy never saw — so `loggingPolicyFor` returned the `metadata` default and the workspace's `none`
was decorative. Gating `storeCaches` on the policy left the test **green**, because the policy was
never reachable from the code under test. The manager is now wired in and the resolved policy is
asserted before anything else runs. A test that proves a privacy claim is broken had better be
reaching the privacy setting.
