# The pair verifier as the pool's gate — measured 2026-09-27 (B9.7)

B9.2 built the verifier and measured it alone (docs/pool-b92-measured.md): 0/86 danger pairs.
Nicolai decided on 25 Sep that pooled rows may keep the contributor's question text. B9.7 wires it.

## The gate as it serves

`cache.SemanticCache.GetPooled`, which serves the buffered and the streamed request alike:

| the asked question | candidate | served |
|---|---|---|
| names an entity | entity gate (equal discriminators) at the 0.98 threshold, unchanged | only on the verifier's YES |
| names none | a stored question that names none either, similarity ≥ **0.60** (`cache.NoEntityLowerBound`) | only on the verifier's YES |

Refused, never served on similarity alone: a row with no question text (every row written before
migration 0135), a pair over 4,000 characters, a verifier error or a 3 s timeout.
Without `LENS_ANTHROPIC_API_KEY` the pooled read stays exactly as before B9.7.

`prompt_embeddings.prompt_text` (0135) is written by `SetPooled` on pooled rows only, exactly when
the pooled response is, lives and dies with its row, and is selected by nothing but the two pooled
lookups. Like the response, it is kept for a poolable workspace whose logging policy is `none` —
the open decision in docs/retention-none-and-the-semantic-cache.md covers both.

## The bound, measured

`PAIRVERIFY=1 scripts/measure-pool.sh` in production's container environment: text-embedding-3-small,
claude-haiku-4-5 at temperature 0, every pair three times (a danger pair YES in any run is served; a
rephrasing is recovered only if YES in all three). `cmd/pairverify` now scores the wired gate
through `cache.PooledCandidateAt`, the same rule the SQL applies.

| no-entity bound | rephrasings served (eng + consumer) | danger served | checks | served / checks |
|---|---|---|---|---|
| 0.60 | **21/68** (3 + 18) | **0/86** | 58 | 36% |
| 0.65 | 18/68 (3 + 15) | 0/86 | 53 | 34% |
| 0.70 | 12/68 (3 + 9) | 0/86 | 43 | 28% |
| 0.75 | 11/68 (3 + 8) | 0/86 | 34 | 32% |
| 0.80 | 6/68 (3 + 3) | 0/86 | 17 | 35% |
| 0.85 | 3/68 (2 + 1) | 0/86 | 7 | 43% |
| 0.90 | 2/68 (2 + 0) | 0/86 | 3 | 67% |
| today (0.98 + entity gate, no verifier) | 2/68 | 0/86 | — | — |

Entity-free consumer rephrasings score 0.55–0.86, so the 0.85 and 0.90 bounds B9.2 suggested
recover 1 and 0 of them. Danger is 0 at every bound, `isa-year` included (it names an entity, so it
stays in the entity lane, where 0.9377 < 0.98). **0.60** serves the most and saves the most net of
checks: 21 × $0.000669 − 58 × $0.000149 = $0.0054 on the corpus, against $0.0041 at 0.65 and
$0.0016 at 0.70. A check costs $0.000149 against $0.000669 for the answer a hit saves (break-even
22.3% of checks); 36% of checks at 0.60 end in a serve.

The corpus holds only near pairs, so production will see more checks per serve than 36%: every
entity-free question whose nearest pooled question scores ≥ 0.60 costs one check.

## Production, before

Read-only, 2026-09-27 00:05 UTC, lens at 0b8ef86: 8 workspaces, all poolable; 7 pooled semantic rows;
pooled serves all time: 6 exact, **0 semantic**. The "after" is in the B9.7 line of BUILD.md.
