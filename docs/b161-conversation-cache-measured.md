# A cached answer never crosses into another conversation — measured 2026-09-27 (B16.1)

## What went wrong

27 Sep, Nicolai's own chat: after "how much is 2+3?" → "2 + 3 = 5" he asked "how much is 2+2?" and
was served "It's still 5. 🙂 …" at 0 LXC — the answer another of his chats got for "so how much is
2+3?". `SemanticCache.Get` embedded the WHOLE conversation and served the workspace's nearest row
above the threshold, with no entity gate and no verifier (those protected pooled rows only). Two
conversations that share their history embed almost identically whatever their last question says.

## The gate now

Both semantic reads — the workspace's own (`Get`) and the pool (`GetPooled`), buffered and streamed
alike — compare a `cache.Turn`: the request's latest user message, and a SHA-256 of every message
before it (`prefix_hash`, migration 0136). A row is served only when ALL hold:

1. the history is byte-identical (`prefix_hash` equal; a first question has the empty history);
2. the entity gate on the two LATEST questions (equal discriminators, similarity ≥ the threshold) —
   or, when the question names no entity, a stored question naming none, similarity ≥ 0.60;
3. the pair verifier says the two latest questions have exactly the same correct answer. Without a
   verifier (no `LENS_ANTHROPIC_API_KEY`) an entity-free question is refused, as on the pooled read.

Similarity is the latest question's alone, so a rephrased first question still hits. A request whose
last message is not a plain-text user message is not compared at all — only the exact cache
(byte-identical conversations) can serve it. Every row written before 0136 has no `prefix_hash` and
is never served again: the semantic cache, private and pooled, starts cold. Seeded pool rows come
back by re-running `lens-seed`.

## Measured

`cmd/pairverify` in production's container environment (a one-off `docker compose run --rm
--no-deps` of the lens image with `/pairverify` mounted; no database touched), text-embedding-3-small
and claude-haiku-4-5 at temperature 0, `LENS_SEMANTIC_THRESHOLD=0.98`, every pair checked three
times: a danger pair YES in ANY run counts as served, a rephrasing counts only if YES in all three.
Each conversation is split by `cache.LatestTurn` on the chat body, exactly as the proxy splits a
request, and scored through `cache.ConversationCandidate`, the rule both reads apply in SQL.

### The multi-turn traps (`poolsafety.ConversationDanger` / `ConversationRephrase`)

| | served |
|---|---|
| danger — Nicolai's two conversations, both directions | **0/2** |
| danger — one change after an IDENTICAL history (3+3 vs 3*3, 12-4 vs 12/4, 15−6 vs 6−15, 7>9 vs 9>7, 5² vs 5³, 7×8 vs 7×9, France vs Spain, shorter vs longer, sadder vs happier, chocolate vs cheese, can vs can't) | **0/11** |
| danger — the SAME follow-up after a DIFFERENT history ("and what about France?", "and in French?", "and in Fahrenheit?", "when was he born?", "what about doubling that?", "make it shorter", "is it safe?", a standalone question vs the same words as a follow-up) | **0/8** |
| **all danger** | **0/21** |
| rephrase — single-turn (rainbow colours, spider legs, boiling an egg) | **3/3** |
| rephrase — same history (kneading dough, a cat's sleep) | **2/2** |

The verifier ALONE, on the two latest questions, would have served 4 of the 21 — "and what about
France?", "when was he born?", "make it shorter" and the standalone-vs-follow-up pair, each
identical words whose answer depends on the history. The history hash is what refuses them.

"which colours are in a rainbow?" after "what are the rainbow colours?": similarity 0.8812, names no
entity, verifier YES 3/3 — served from the workspace's cache.

### The single-turn corpora, unchanged

The same run re-measured B9.7's corpora through the same rule: engineering 3/30 rephrasings served,
0/44 danger; consumer 18/38 served, 0/42 danger. **Extended corpus: 0/107 danger served.**

### Through the real handler

`internal/proxy/b161_conversation_cache_realpg_test.go` sends Nicolai's two conversations and a
follow-up after another history through the real handler to real pgvector, on the private path and
the pooled one, with the embedder rigged to give every text the same vector and the verifier rigged
to say YES — so only the history hash and the entity gate can refuse. Every one goes to the model;
a rephrased first question is served from the cache. On main before this change the same test
serves "how much is 2+2?" the answer to "so how much is 2+3?", privately and through the pool.

## The operator stop-gap

`LENS_SEMANTIC_THRESHOLD=1.0` has been in the server's `.env` (line 93) since 27 Sep. It closes the
entity lane only; the entity-free lane (0.60 + verifier) never read the threshold. Once this merge is
deployed, restore 0.98 with:

```sh
ssh root@65.108.249.225 'cd ~/talyvor-lens && sed -i "s/^LENS_SEMANTIC_THRESHOLD=1.0$/LENS_SEMANTIC_THRESHOLD=0.98/" .env && docker compose up -d lens'
```
