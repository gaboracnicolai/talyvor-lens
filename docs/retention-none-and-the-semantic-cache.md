# `logging_policy = none` stores no content — decided 27 Sep 2026, built in B18.4

## What it means

A workspace whose `logging_policy` is `none` has **no prompt or response content stored**:

- **no exact cache** entry (Redis) — private or pooled;
- **no semantic cache** row (`prompt_embeddings`) — private or pooled;
- **no pool contribution** of any kind, including shared document conversions;
- **no document conversion** cached (the private distill cache is read-only for it);
- **no prompt template** recorded (`prompt_templates` keeps a system prompt's text), so it forgoes
  the pinned prompt-caching rewrite;
- **no content logs**: `token_events`, session turns, request attribution, the learner stream and the
  pool shadow log were already skipped for `none`.

It **may still be served from the shared pool** when it has opted into sharing: reading stores
nothing. What it gives up is its own cache — every repeat of its own question goes to the model, so
**its bill goes up**. That is the trade Nicolai decided on 27 Sep.

## How it is enforced

`Proxy.storeCaches` returns before writing anything when the workspace resolves to `none`. It is the
one choke point for every cache write — the buffered path, local and node routing, and the streamed
path all call it — so no call site can forget. The distill integration makes the private conversion
cache read-only and skips both pooled writes for `none`; the template recorder is skipped for `none`.

Since B26.7 the reads match: `Proxy.tryExact` and `Proxy.trySemantic`, the workspace's own exact and
semantic reads, return nothing for `none`, so an answer kept **before** a switch to `none` is never
replayed — the repeat goes to the model and is charged as a model call, buffered and streamed. The
pooled reads are untouched. Proved by `internal/proxy/b267_logging_none_no_replay_realpg_test.go` on
`lxc_ledger`.

## How it is proved

`internal/proxy/b184_logging_none_realpg_test.go`, through the real handler, with the exact cache, the
semantic cache and the template recorder wired to a fully migrated Postgres: a `none` workspace asks
a question carrying a unique marker in its system prompt and its question, and the answer carries it
too. The marker is then searched for in **every text, json and array column of every table** and in
**every Redis key and value** — zero hits. A control proves the scan sees content: the same kind of
question from a `metadata` workspace IS found. The `none` workspace is then served another
workspace's pooled answer, with no model call, and owns no `prompt_embeddings` row and no exact
entry afterwards.

On the code before B18.4 the same test found the `none` workspace's content in
`prompt_templates.content`, `prompt_embeddings.response` and `prompt_text` (private and pooled), and
two exact-cache entries it owned.

## History

This file previously recorded the measurement that `none` did NOT stop the semantic cache (#461,
W4.6.2), with executable tests pinning that fact and a request for the decision. The decision is
taken; those tests were deleted with the behaviour they described, as their own failure messages
instructed.
