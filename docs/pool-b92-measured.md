# The pair verifier, measured on the committed corpora — 2026-09-25 (B9.2)

**The verifier:** `internal/pairverify`. It asks a small model one question about the pair — "would
these two questions have exactly the same correct answer?" — and allows a serve only on a bare
YES. It is wired to nothing.

**The harness:** `cmd/pairverify`. It ran in production's lens container environment, using
production's Anthropic and OpenAI keys, with no database access. Model `claude-haiku-4-5` at
temperature 0 (set `LENS_PAIRVERIFY_MODEL` to measure another). Every pair was checked **three
times**:
- a danger pair answered YES in **any** run counts as served;
- a rephrasing counts as recovered only if **all three** runs said YES.

The corpora are `poolsafety.ByTraffic()`: 68 rephrasings and 86 danger pairs (the 20 held-out
engineering danger pairs included).

## Result

| lane | danger pairs served (YES in any of 3 runs) | rephrasings recovered (YES 3/3) | YES in only some runs |
|---|---|---|---|
| engineering | **0/44** | 13/30 (43%) | 0 |
| consumer | **0/42** | 24/38 (63%) | 0 |
| **total** | **0/86** | **37/68 (54%)** | **0** |

**Danger = 0**, so the item's condition for wiring holds. No answer changed between runs: every
pair got the same verdict three times.

**For comparison, the live gates (B9.1).** Similarity ≥ 0.98 plus the entity gate recovers 2 of
the 68 rephrasings. The verifier alone, over every pair, recovers 37.

**As a second gate after the live gates**, as B9.2 specifies: today's gates admit 2 rephrasings
and 0 danger pairs. The verifier passes both rephrasings and serves no danger pair. Placed after
0.98, it would therefore **change nothing** on this corpus. Its value only appears if the
candidates come from something looser than 0.98, and that would be a threshold change, which is
not this item's to make.

**Rephrasings the verifier refused in every run:**
- engineering: bash-env, c-segfault, css-center, docker-slow-build, go-channels, go-defer, go-errors, jest-mock, js-let-var, js-triple-equals, linux-large-files, memory-leak, node-econnrefused, pytest-basics, race-condition, react-rerender, sql-joins
- consumer: burn-first-aid, car-battery, compound-interest, dog-walk, interview-prep, laptop-slow, learn-guitar, password-strong, pension-start, photosynthesis, plant-dying, reset-router, tax-deadline, visa-japan

A refusal costs a hit, never a wrong answer.

## Cost

| | tokens (mean) | cost on claude-haiku-4-5 |
|---|---|---|
| one check | 129 in + 4 out | **$0.000149** |
| the answer a hit saves (10 real generations, same model) | 39 in + 128 out | $0.000679 |

**Break-even hit rate: 21.9%.** At least that share of checked candidates must end in a serve for
the checks to pay for themselves. This is the conservative case: the answer is priced on Haiku,
and a pool that serves answers from a larger model saves more per hit.

## Why it is not on the serve path yet

To judge a pair, the verifier needs **both questions**. The pooled table keeps no question text,
and that is deliberate:
- `prompt_embeddings` stores the embedding, the response, the discriminators and a hash;
- migration 0125 stores "no prompt text";
- the B1.3 chat history is browser-local because "Lens stores no prompt text";
- the pooled SQL fails legacy rows closed because "a row whose prompt text no longer exists is never served".

Wiring the gate therefore means **keeping each contributor's question text on its pooled row**.
That reverses a privacy design, so it is Nicolai's decision, not a build step. And measured, the
gate would change nothing behind today's 0.98 threshold.

**Decision needed:** may pooled rows keep the contributor's question text? This would apply only
to workspaces that opted into pooling, and each check sends both questions to the verifier model.
- If **yes**, the wiring is: a nullable `prompt_text` column on `prompt_embeddings`, written by `SetPooled` and read by `GetPooled`; the verifier after the threshold in `trySemanticPooled`, which serves both the buffered and streamed seams; a row with no text is refused.
- If **no**, the verifier stays a measurement.

Re-run: `PAIRVERIFY=1 scripts/measure-pool.sh <ssh-target>`.
