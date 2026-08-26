# Model 2 — what the allowance economics actually require

**W4.6.1 step 2 · tab-q4vn · 2026-08-26 · measured, nothing tuned**

The item says: *"F AND D ARE NICOLAI'S. Build it with configurable values and a documented
default; report what the economics require at the measured hit rate (h≈0 today) versus what
they would allow at h=0.3."*

This is that report. **No price is set here.** `LENS_SUBSCRIPTION_ALLOWANCE_ULXC` defaults to
**0** — "no allowance priced yet" — and the Stripe Price (F) lives in Stripe.

---

## §01 The one measurement everything else follows from

> **LXC is sold at cost and spent at cost. The gross margin on metered LXC is zero by
> construction.**

Both halves, read from the code at `f06ab5f`:

**Sold at cost** — `internal/billing/billing.go#lxcForCents`:
```go
return int64(math.Floor((float64(usdCents) / 100.0) / economy.LXCUSDValue * 1e6))
```
$10.00 buys exactly 100 LXC at the `LXCUSDValue = 0.10` peg. There is no spread on a fiat
purchase. (`ConversionSpread = 0.05` exists, but it is the LENS→LXC *conversion* margin — a
different path, not a fiat top-up.)

**Spent at cost** — `internal/proxy/proxy.go:480`, `lxc_gate.go:101,128`:
```go
return int64(math.Ceil(usd / economy.LXCUSDValue * 1e6))
```
…where `usd` is `alerts.CostUSDResolved(...)`, priced from `internal/catalog`. And the catalog
holds **provider list prices**, not marked-up ones — `seed.go:88`:
```
claude-haiku-4-5 · InputPer1M: 1.00 · OutputPer1M: 5.00
```
Those are Anthropic's published Haiku 4.5 rates.

So **1 LXC ≡ $0.10 of provider COGS**, on both sides of the ledger. A subscriber who consumes
D µLXC costs Talyvor exactly:

```
COGS_worst(h=0)  =  D × 1e-7  USD          (D in µLXC)
```

⚠ This is the number the rest of this report turns on, and it is the one to re-measure if
anyone changes the peg, the catalog's provenance, or `lxcForCents`.

---

## §02 The inequality, made explicit

The item states `phi = F/D < 1` — *"the allowance is worth more than the fee"*. Making the
units explicit (F in USD, D in µLXC, `D_usd = D × 1e-7`):

```
phi = F / D_usd  <  1        ⟺        F < D_usd
```

Combine with §01. With a pool hit rate `h` (a pooled hit costs no provider spend):

```
COGS_worst(h)  =  D_usd × (1 − h)
margin_worst   =  F − D_usd × (1 − h)
```

**Worst case breaks even when:**

```
F  ≥  D_usd × (1 − h)
⟺   phi  ≥  1 − h
⟺   h    ≥  1 − phi
```

That is the whole model in one line: **the hit rate must cover the discount.** If you give
away 2× the fee in allowance (phi = 0.5), half of all traffic must be served from the pool
just to break even *in the worst case*.

| phi = F/D_usd | the offer | h required for worst-case break-even |
|---|---|---|
| 0.50 | allowance worth 2× the fee | **h ≥ 0.50** |
| 0.67 | 1.5× | **h ≥ 0.33** |
| 0.70 | ~1.43× | **h ≥ 0.30** |
| 0.80 | 1.25× | **h ≥ 0.20** |
| 0.90 | 1.11× | **h ≥ 0.10** |
| 1.00 | allowance = fee | h ≥ 0 (no pooling needed) |

---

## §03 The measured h, and what it does to the table

**h is structurally zero in production today.** Not "low" — zero, and for a structural reason.

| source | measurement |
|---|---|
| **W2.1** (`reports/report-w21-pooling-hit-rate.md`) | *"hit rate is **0% at all four thresholds**"*; *"hit rate is **structurally zero**: the production pool…"* — read against live production, 2026-08-09 |
| **W2.6** (`9d70943`) | the tier-2 canonical form serves **1 genuine rephrasing in 68**, over two independent runs (2×592 live calls, 296 distinct prompts) |
| **W2.7** (`6b1f99d`, three merges) | doc2query buys **ZERO** served rephrasings on both lanes, at every variant count and every threshold — and the consumer lane's zero is *by construction* |

The most generous defensible reading is the W2.6 corpus figure, **h ≈ 1/68 ≈ 0.015**, and that
is a controlled corpus, not production. Production is 0.

Putting that into §02:

```
h = 0     ⟹   worst case breaks even only when phi ≥ 1
```

> ### ⚠ At the measured hit rate there is NO phi < 1 whose worst case breaks even.
>
> `phi < 1` is the item's own premise, and at h = 0 it guarantees a worst-case loss of
> `D_usd − F` per subscriber per period. That is not a defect in the ledger — the ledger
> bounds the loss at exactly `D_usd − F`, which is the entire point of the hard cap. It is a
> statement about what the offer costs.

At the item's aspirational **h = 0.3**, the table says **phi ≥ 0.7** is worst-case safe — i.e.
the allowance may be worth up to ~1.43× the fee and still not lose money even if every
subscriber consumes all of it. That is a real, comfortable product. **It is also ~20× the best
hit rate anyone has measured on this system, and W2.7 has just reported that the last untried
recall mechanism buys zero.**

---

## §04 So what makes Model 2 work, if not h?

Three candidates. Only the first is available today, and it is the one every allowance-based
SaaS actually runs on.

1. **Average consumption far below D.** The worst case is `D_usd`; the *expected* case is
   `E[consumption]`. The business is fine whenever `F > E[consumption]_usd × (1 − h)`, which
   for a typical long-tail usage distribution is satisfied at phi well below 1. **This is the
   real bet, and it should be stated as one.** The hard cap is what makes the tail bounded
   rather than unbounded — that is precisely its job.
   ⚠ **THE NUMBER NOBODY HAS: the consumption distribution.** There are no subscribers, so
   `E[consumption]` is unmeasured and unmeasurable today. **The allowance ledger built in this
   merge is the instrument that will measure it** — `consumed_ulxc / granted_ulxc` per period,
   per subscriber, is exactly that distribution.
2. **A hit rate that does not currently exist.** W2.1/W2.6/W2.7 are three independent
   measurements saying so. Do not price against h > 0 until something moves it.
3. **Selling LXC above cost.** Currently forbidden by construction (§01): the peg makes
   purchase and spend the same number. Introducing a retail margin would change every number
   in this document, and it is a pricing decision, not an engineering one.

---

## §05 What to do with D and F — for Nicolai, not decided here

- **Pick phi from risk appetite, not from h.** At h = 0, `phi` *is* the worst-case loss ratio:
  a subscriber who burns the whole allowance costs you `D_usd`, and you collected `F`.
- **Then pick D from the worst case you can fund**, because the hard cap makes that exact:
  worst case per subscriber per period = `D × 1e-7` USD, full stop. Ten thousand subscribers
  at D = 500 LXC is a bounded exposure of `10,000 × 500 × $0.10 = $500,000` per period, and
  the ledger guarantees it cannot be more.
- **Set `LENS_SUBSCRIPTION_ALLOWANCE_ULXC` only when both are chosen.** With it at 0 the
  mechanism is inert and a subscription entitles the buyer to nothing — which is visible
  immediately, and is the intended state of an undecided price.

⚠ **RE-MEASURE §01 BEFORE TRUSTING ANY OF THIS.** Every number here rests on "1 LXC = $0.10 of
provider COGS on both sides". If the peg, the catalog's provenance, or `lxcForCents` changes,
this document is wrong and says so rather than quietly drifting.
