# Tare phase 1a — the JSON compressor, measured

W6.1.1. The item asks for a `Reduction` interface, a deterministic in-house lossless JSON
compressor, and — the part that matters — **the measured reduction per corpus, said honestly**:

> ⚠ IF IT IS NEAR ZERO ON REAL PAYLOADS, SAY SO — the previous compressor measured 0.000% over 308
> prompts and shipped anyway.

## The headline: it is not near zero, and the previous 0.000% has an explanation

**21.62%** over the real JSON in this repo. **63–68%** on the shape a tool output actually has.
**0.000%** on the corpus the item pointed at — and that last number is a fact about the corpus, not
about the compressor.

## Corpus 1 — `internal/poolsafety`: 0.000%, structurally

```
poolsafety prompts: 252 entries, 0 parsed as JSON, 0 reduced, 9367 -> 9367 bytes (0.000%)
```

W6.1.1 says to measure on "the committed corpora in internal/poolsafety". Those corpora are
**prompt pairs** — short natural-language questions built to test embedding similarity. **Zero of
the 252 entries parse as JSON.** A JSON compressor scores exactly zero on them by construction.

⚠ **This is very likely what the previous compressor's "0.000% over 308 prompts" was.** A number
without its population is not a measurement, and "0.000% over 308 prompts" reads as *the compressor
does nothing* when it may only have meant *this corpus contains nothing it is for*. The test that
produces the line above therefore asserts the composition first, and fails if the corpus ever
becomes partly JSON without someone re-reading the result.

## Corpus 2 — real JSON in this repo: 21.62%

```
values.schema.json      8083 ->   8083    0.00%  REFUSED
lens-economy.json       2788 ->   1787   35.90%
lens-overview.json      3672 ->   2153   41.37%
lens-providers.json     3156 ->   2067   34.51%
sample.json               65 ->     65    0.00%  REFUSED
facts.json               728 ->    387   46.84%
package-lock.json     138912 -> 108836   21.65%
TOTAL                 157404 -> 123378   21.62%
```

⚠ **Population boundary, stated rather than implied.** These are Grafana dashboards, a Helm values
schema, distill fixtures and an npm lockfile. They are real JSON, but **they are not agent tool
output**, which is what Tare would actually see. This repo captures no gateway traffic, so the
representative corpus **does not exist yet**. The figure above indicates the transform's behaviour
on dict-array-heavy JSON; it is **not a forecast of production saving**. Capturing that corpus is
the first thing the metering phase needs.

Both refusals are honest ones: `values.schema.json` is a JSON *schema* — deeply nested objects, no
array of same-shaped objects — and `sample.json` is 65 bytes.

## Corpus 3 — by shape, so the number can be read rather than quoted

```
tool output: 20 same-shaped rows           1694 ->   611   63.93%
tool output: 200 same-shaped rows         17331 ->  5628   67.53%
3 rows, short keys                           43 ->    43    0.00%
3 rows, long keys                           178 ->   100   43.82%
mixed shapes (refused)                       31 ->    31    0.00%
scalar array (refused)                       22 ->    22    0.00%
deeply nested object, no arrays (refused)    31 ->    31    0.00%
```

The saving is the repeated keys, so it scales with **rows × key-name length** and is zero when
either is small. On the shape a linter, a grep or a file listing actually returns — many rows, the
same descriptive keys on each — it is about two thirds.

## What the transform is

One rewrite, invertible by construction: an array whose elements are **all** objects with the
**identical key set** becomes a table.

```json
[{"file":"a.go","line":1},{"file":"b.go","line":2}]
{"~tare":1,"cols":["file","line"],"rows":[["a.go",1],["b.go",2]]}
```

Columns identical in every row are hoisted once into `const`. `ExpandJSON` is the exported inverse —
a transform whose inverse is not runnable is one nobody can check, and every test round-trips
through it.

### Three decisions worth inheriting

- **Identical key sets, not a union with nulls.** `{"a":1}` is not the same document as
  `{"a":1,"b":null}` — an agent that branches on presence reads them differently. Mixed shapes are
  **refused**. That is the difference between lossless and lossless-in-practice.
- **Numbers keep their literals.** Decoding JSON into `any` makes every number a `float64`, which
  destroys integers above 2^53 and rewrites `1.0` as `1` — silently lossy on ids, counts and hashes.
  This package decodes with `UseNumber()`.
- **It never returns something larger.** On a short array the table header costs more than the keys
  it removes, so the result is measured and the input returned unchanged when it does not win.

### What is not preserved, said plainly

Object member **order** and insignificant whitespace. Neither is meaning (RFC 8259: an object is an
unordered collection) — but neither is byte-identity, so **a caller that hashes raw request bytes
must hash before reduction, not after**. Prefix stability (a later phase) depends on the ordering
being *stable*, which it is: keys are emitted sorted.

## ⚠ One instruction is vacuous in this phase, and pretending otherwise would be the defect

W6.1.1 says: *KEEP errors, outliers, and first-and-last elements.* It also says **LOSSLESS ONLY**.
Under a lossless transform nothing is dropped at all, so the keep-rule **cannot fail** — it is
satisfied by construction, not by any code I wrote. It is asserted anyway
(`TestJSON_KeepsEveryElementIncludingErrorsOutliersFirstAndLast`) so that the moment any elision is
added in a later phase, the guard the item asked for is already there and stops being vacuous.

## Controls

`w611-tare-controls-k7v3.py` — **7/7 CAUGHT**. The ones worth reading found problems in the *tests*:

- **H6** — the token-estimate assertions were nearly vacuous. The original check was only
  `tokensOut > tokensIn`, which a constant-zero estimator satisfies trivially. Both ends are pinned
  now.
- **H5 corrected its own premise, and exposed a vacuous test.** I expected removing the key sort to
  make output vary run to run. It does not — `json.Marshal` sorts map keys, so byte-stability
  survives. What breaks is **shape matching**: element 0's map-iteration order stops equalling
  element 1's, every array is judged mixed-shape, and the reducer silently reduces **nothing**. And
  `TestJSON_IsDeterministic` passed anyway — because "the same bytes twenty times" is trivially true
  of a reducer that refuses. Worse, its original input (`a`/`b`/`c` keys) had **never been reduced
  at all**: the table header cost more than three one-character keys, so the test had been asserting
  the stability of a no-op since it was written. It now asserts that a reduction happened, over keys
  long enough for the table to pay.

## What phase 1a deliberately does not do

**Nothing calls this.** W6.1.1: "WIRED TO NOTHING. This merge adds no call site on the serve path."
There is no router, no metering record, no attribution, and no economy wiring — those are 1b–1e, and
the design is explicit that the saving-to-credits path ships **default-off / rate-0** behind an
external audit gate.
