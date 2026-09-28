# A rephrasing that names something is answered from the cache — measured 2026-09-28 (B21.1)

Measured 27 Sep in Nicolai's chat: "which city is the France's capital?" after "what is the capital
of France?" went to the model, and so did "how much is 3+3?" after "what is 3+3?". A question that
names something or carries a number takes the **entity lane**, and that lane's floor was the 0.98
threshold, so a rephrasing never became a candidate and the pair verifier was never asked. Only
entity-free questions had a lower candidate bound (`NoEntityLowerBound`, 0.60, B9.7).

## The gate as it serves now

`cache.SemanticCache.Get` (private) and `GetPooled` (pooled), which serve the buffered and the
streamed request alike:

| the asked question | candidate | served |
|---|---|---|
| names an entity or a number | the **identical history**, **exactly equal** entities and numbers on the two latest questions, similarity ≥ **0.60** (`cache.EntityLowerBound`; was 0.98) | only on the verifier's YES |
| names none | the identical history, a stored question that names none either, similarity ≥ 0.60 | only on the verifier's YES |

Without a verifier (no `LENS_ANTHROPIC_API_KEY`) the entity lane stays at the threshold, as before.
Possessives were already canonical — "France's" is `propn:france`, "UK's" is `caps:uk` — and
`TestCanon_APossessiveNamesTheSameEntity` holds it.

## Measured

`cmd/pairverify` in production's container environment (text-embedding-3-small, claude-haiku-4-5 at
temperature 0), touching no database. Every pair three times per run: a danger pair answered YES in
ANY run counts as served; a rephrasing counts only when YES in every run. The private and pooled
reads apply the same rule (`cache.PooledCandidate` / `ConversationCandidate` / `StandaloneCandidate`),
so one measurement is both paths'. The whole measurement was run three times; the three agree to the pair.

| corpus | danger served (each run) | rephrasings served at 0.60 |
|---|---|---|
| engineering (B9.2) | 0/44 | 7/30 (was 3/30 with the entity lane at 0.98) |
| consumer (B9.2) | 0/42 | 18/38 |
| **names and numbers (B21.1, new)** | **0/18** | **18/18** |
| conversations (B16.1) | 0/21 | 5/5 |
| standalone mid-conversation (B16.2) | 0/36 | 7/7 |

B9.2's 86 danger pairs, B16.1's 21 multi-turn traps and B16.2's 36: none served, in any run.

**Nicolai's eight questions** are all served, YES 3/3 in every run: UK capital (similarity 0.8554),
France capital (0.7996), 3+3 (0.9115), 3+4 (0.9123). Every one sat under the old 0.98 floor.

**The new danger pairs are the ones the entity gate cannot refuse** — the same names and numbers, a
different answer: capital vs largest city of Australia and France, Python/Java and Tokyo/London
direction, enable/disable SSO for Okta, 3+3 vs 3×3 and 3−3, USD→EUR vs EUR→USD, upgrade vs downgrade
Python 3.11↔3.12, Obama born vs elected. 17 of the 18 pass the entity gate; the verifier refused
every one in every run. In the multi-turn corpus the lower bound newly admits 3+3 vs 3×3, 12−4 vs
12/4, 15−6 vs 6−15, 7>9 vs 9>7 and 5² vs 5³ as candidates; the verifier refused each 0/3.

### Where the entity bound belongs

The entity lane swept with the no-entity lane at 0.60 (run 1):

| entity bound | engineering served | names-and-numbers served | danger served (all flat corpora) | checks |
|---|---|---|---|---|
| **0.60** | 7/30 | **18/18** | 0/104 | 105 |
| 0.70 | 6/30 | 18/18 | 0/104 | 103 |
| 0.80 | 4/30 | 16/18 | 0/104 | 86 |
| 0.90 | 3/30 | 12/18 | 0/104 | 77 |
| 0.95 | 3/30 | 5/18 | 0/104 | 69 |

Danger is 0 at every bound, so 0.60 — where it started, `NoEntityLowerBound` — serves the most.
On the names-and-numbers corpus 53% of checks end in a serve, against a 21.3% break-even (a check
costs $0.000149; the answer it saves, on the cheapest model, $0.000699).

Reproduce: `PAIRVERIFY=1 scripts/measure-pool.sh <ssh-target>`, or build `./cmd/pairverify` and run it
in the lens container as that script does.
