# Tare phase 1c — prefix stability, verified against the live provider

W6.1.3, the item the Tare set calls the one it cannot do without:

> Without it a measured token saving moves the bill **nowhere**, and an economy that books an
> illusory saving mints credit from nothing.

> ⚠ AND MEASURE THE PROVIDER'S CACHE BEHAVIOUR IF YOU CAN … **Report the number; do not assume it.**

## The number, measured against Anthropic's live API

Three real requests to `claude-haiku-4-5`, identical system prompt, using the same
`cache_control: {type: ephemeral}` shape this repo already sends
(`internal/templates/detector.go#ApplyAnthropicCaching`, live from the template-detection block in `internal/proxy/proxy.go`):

| # | request | `input_tokens` | `cache_creation` | `cache_read` |
|---|---------|---------------:|-----------------:|-------------:|
| 1 | warm the cache | 7 | **5702** | 0 |
| 2 | next turn, **full** 3073-byte tool output | 1016 | 0 | **5702** |
| 3 | next turn, **Tare-reduced** 1122-byte payload | **564** | 0 | **5702** |

**Verdict: the prefix held.** The reduced request read **exactly the same 5702 cached tokens** as
the unreduced one, while uncached input fell **1016 → 564**.

**452 tokens saved (44.5% of the uncached input), and zero cached tokens lost.** For this shape the
layer is net-positive — the saving is real, not cancelled by a cache miss.

⚠ **Scope of that claim, stated:** Anthropic, `claude-haiku-4-5`, one payload shape, one run,
~$0.01. It is a demonstration that prefix stability *works against a real provider cache*, not a
population estimate. OpenAI was not measured; its caching is automatic and its rule — stated in this
repo's own `ApplyOpenAICaching` comment — is the same one enforced here.

⚠ **The first run of this probe reported `cache_read=0` for both arms and that was the probe, not
the product.** Its system prompt was ~450 tokens and Anthropic's minimum cacheable span is 2048 on
Haiku. Reporting that zero would have been publishing a broken instrument as a product result — the
same failure this Tare set already records about "0.000% over 308 prompts". The system prompt was
enlarged past the minimum and the cache engaged immediately.

## What "the frozen prefix" is in this product — measured, not assumed

It is **the system prompt**, and both providers say so in this repo's own code:

- **Anthropic** — `ApplyAnthropicCaching` puts `cache_control: ephemeral` on the system block, and
  Anthropic caches everything up to and including the marked block. Live, from the
  template-detection block in `internal/proxy/proxy.go`,
  whenever a system prompt is extractable, the template is **pinned**, and the provider is anthropic.
- **OpenAI** — `ApplyOpenAICaching` is deliberately a no-op whose comment states the rule directly:
  caching "kicks in as long as the system message stays **first and byte-identical**."

So the rule is not a generic "compress the first N bytes". It is: **Tare must never touch the system
prompt, and must not disturb any message but the newest.**

## How it is enforced

`PrefixStable` wraps any `Reduction` and applies it **only to the newest message's content span**,
located by scanning tokens with `json.Decoder.InputOffset()` and **spliced** — never unmarshalled and
re-marshalled.

⚠ **Splicing is the point, not a style choice.** Re-serialising would reorder the envelope's keys and
restyle its whitespace. Even where a provider tokenises *content* rather than raw HTTP bytes — so
reordering is *probably* harmless — "probably harmless" is exactly the reasoning W6.1.3 forbids:
**assert the bytes, not the intent.** Splicing makes every byte before the newest message unchanged
*by construction*, and the reducer re-checks that construction on the body in front of it before
returning; a prefix that moved is a **refusal**, never a silently-shipped cache miss.

**History is never dropped, merged or summarised.** The only thing that changes is the content of the
last message.

## Controls — 5/5 CAUGHT

`w613-prefix-controls-k7v3.py`.

⚠ **J3 is the control W6.1.3 demands by name**: *"break the prefix by one character and confirm the
test reds."* It alters exactly one byte of the frozen prefix; the guard refuses and the test goes
red. One character is all a provider's cache needs to miss.

- **J1** re-serialises the envelope instead of splicing. The transform still "works" and the decoded
  document is identical — and the prefix has moved. This is why the assertion is on bytes.
- **J2 exposed a vacuous test.** Pointing the span finder at the *first* message makes the inner
  reducer refuse (turn 0 is prose), so the whole thing refuses, the body comes back unchanged — and
  "nothing before the newest message changed" is then **trivially true**.
  `TestPrefix_OnlyTheNewestMessageChanges` now asserts that a reduction actually happened. That is
  the third time in this Tare work a prefix/stability assertion turned out to be satisfied by a
  no-op.
- **J4** splices the reduced content raw instead of re-encoding it as a JSON string, losing the
  phase-1a round-trip guarantee inside the envelope.
- **J5** makes the reduction grow the body; it must be refused.

## Wired to nothing

No call site on the serve path. The metering record and the holdout are phase 1d.
