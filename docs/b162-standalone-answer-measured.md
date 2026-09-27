# A question asked mid-conversation reuses a standalone answer when it stands alone — measured 2026-09-27 (B16.2)

## What changed

B16.1 made a cached answer serve only the same question asked after the IDENTICAL history, so
"what are the rainbow colours?" asked after an unrelated exchange went to the model although the
same question had been answered on its own.

Now, when the same-history lookup misses and the question was asked mid-conversation, both semantic
reads (the workspace's own, then the pool; buffered and streamed alike) look once more — at rows
stored for a question asked ON ITS OWN (empty history, and the request fingerprint the latest message
would have alone, `cache.Turn.AloneFP`). Such a row is served only on TWO YESes:

1. the pair check (`pairverify.Prompt`, B9.2/B9.7): the two questions have exactly the same answer;
2. the stands-alone check (`pairverify.StandalonePrompt`), shown the conversation and the new message
   but NOT the stored question: would the correct reply be exactly the same had the conversation
   never happened?

Candidates are chosen by the same rule as every other lane (entity gate at the threshold; with no
entity, similarity ≥ 0.60). The lane is closed — the request goes on as before — for a first
question, a history containing a system or tool message or non-text content, a history over 4,000
characters (not cut: the part cut could be the part the question depends on), or a verifier without
the stands-alone check (no `LENS_ANTHROPIC_API_KEY`).

## How the check was arrived at — three measurements

All three: `cmd/pairverify` in production's container environment (a one-off `docker compose run
--rm --no-deps` of the lens image, no database touched), text-embedding-3-small, claude-haiku-4-5 at
temperature 0, every pair three times; a danger pair YES in ANY run counts as served.

| design | danger served | rephrasings served |
|---|---|---|
| ONE prompt judging "stands alone" and "same answer as Question A" together | **10/30** | 5/5 |
| pair check + "could someone who has NOT seen the conversation answer it correctly?" | **1/30** | 4/5 |
| pair check + "would the correct reply be exactly the same had the conversation never happened?" | **0/36** | **7/7** |

The first design judged the pair, not the conversation: shown "and what about France?" as both
questions it said YES after "what is the population of Germany?", and "how do I say hello?" after
"I'm learning Spanish". The second still passed the Spanish one. The rephrasing it refused ("how long
should I boil an egg?" after "I'm practising mental arithmetic, quiz me back after each answer") was
refused rightly — that history changes the right reply — so it is now a danger pair, and a rephrasing
after a neutral exchange replaced it. Five further traps (an Iceland wedding, tipping in Tokyo,
counting to ten for a German learner, a London–New York time difference, naming a black kitten) were
written before the third design was measured; they and the relabelled egg were all refused.

## The final measurement — the whole extended corpus

One run of `cmd/pairverify` at `LENS_SEMANTIC_THRESHOLD=0.98`, every section, every pair three times:

| corpus | danger served | rephrasings served |
|---|---|---|
| engineering, single-turn (B9.7 gate) | 0/44 | 3/30 |
| consumer, single-turn (B9.7 gate) | 0/42 | 18/38 |
| multi-turn traps, same-history lane (B16.1) | 0/21 | 5/5 |
| standalone lane (B16.2): 15 traps + every B16.1 danger pair re-cast with its stored question asked on its own | 0/36 | 7/7 |
| **all** | **0/143** | |

"what are the rainbow colours?" after "what is the capital of the UK?" → "London.": pair check YES,
stands-alone check YES, in all three runs — served from the standalone answer; so is "which colours
are in a rainbow?" (similarity 0.8812). Every context-dependent follow-up of the B16.1 corpus —
"and what about France?", "when was he born?", "make it shorter", "is it safe?", "and in French?",
"and in Fahrenheit?", "what about doubling that?" — tried against a standalone answer to the very
same words, goes to the model in all three runs.

Through the real handler, `internal/proxy/b162_standalone_mid_conversation_realpg_test.go` stores
the rainbow question on its own and asks it after an unrelated exchange, privately and from another
workspace through the pool: served from the standalone answer, and the stands-alone check was shown
the conversation. A follow-up with identical words goes to the model. On main the rainbow question
goes to the model on both paths.

## Cost and latency

A standalone candidate costs up to two small-model calls: the pair check (~130 input tokens) first,
and only on its YES the stands-alone check, whose input grows with the history (at most 4,000
characters). Worst case, a mid-conversation question that ends in a miss runs a same-history and a
standalone lane on the private read and again on the pooled one — up to six checks, each bounded by a
3-second timeout. Every refusal, error or timeout sends the request to the model, as before.
