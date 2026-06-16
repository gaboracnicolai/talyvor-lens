# DISTILL conversion proof — savings × answer-quality validation

**This is a VALIDATION, not a new capability.** DISTILL is complete; this proof
establishes that its document→markdown conversion delivers its headline claim —
fewer tokens/cost **without losing the information a correct answer needs** —
across the model-free converters and the three fidelity tiers.

Reproduce: `go test ./internal/distill/ -run TestConversionProof -v`
Source: `internal/distill/conversion_proof_test.go` (test-only; no production surface).

## The claim — bounded

**Proven:** across the model-free converters (HTML, CSV, JSON, XML, text), the
**faithful** and **structured** tiers preserve **100%** of curated
answer-relevant reference facts; the **outline** tier preserves only **15%** (it
is lossy by design), paired with the existing honest savings (`TokensSaved` as
computed by `computeSavings` — never recomputed rosier).

**The quality signal is reference-based information preservation:** each synthetic
fixture ships a curated set of answer-relevant facts (values a correct answer
would need), placed in body prose / table / data cells. The metric per tier is the
fraction of those facts that survive (normalized presence-check) in that tier's
distilled markdown. It is deterministic and **model-independent**: a fact absent
from the distilled text cannot be answered by *any* model, so its loss is real.

**NOT proven (stated, not buried):**
- **NOT "a model's answer is unchanged."** Fact preservation is *necessary* but not
  *sufficient* for an unchanged answer. A live-model eval is non-deterministic and
  out of scope.
- **NOT "conversion adds nothing wrong."** The check is **one-directional** — it
  confirms facts did not vanish, not that no spurious/corrupted content appeared.
- **NOT a `ScoreResponse` judgment.** `ScoreResponse` (`quality/scorer.go`) is a
  shallow heuristic (length ratio, refusal phrases, truncation, repetition, a
  markdown-structure bonus); it never compares prompt content to response content,
  so a well-formed *wrong* answer scores ~1.0. It is a weak judge and is
  deliberately not used.

## The OCR / DOCX / XLSX / PDF path is UNPROVEN here — and is the higher-risk path

This harness covers only the **model-free** converters. **Vision-OCR is the
lossiest conversion** (a model reads pixels) and is exactly where savings-quality
risk is highest. It is excluded **only because it needs a vision model**
(non-deterministic, not CI-safe) — **not** because it is low-risk. **Its exclusion
is not coverage:** it requires a separate model-based evaluation before any
savings-quality claim is made about it.

## Result — per fixture (savings paired with fact preservation)

`$ saved` = saved tokens priced at `gpt-4o` input rate (`alerts.CostUSD`), illustrative.

| fixture | format | tier | tokens saved | savings % | $ saved | facts kept | verdict |
|---|---|---|---:|---:|---:|:---:|---|
| html_report | html | faithful | 12 | 30.0 | 0.000030 | 3/3 | ok |
| html_report | html | structured | 12 | 30.0 | 0.000030 | 3/3 | ok |
| html_report | html | outline | 22 | 55.0 | 0.000055 | 1/3 | **DEGRADING** |
| html_contract | html | faithful | 13 | 26.5 | 0.000032 | 3/3 | ok |
| html_contract | html | structured | 13 | 26.5 | 0.000032 | 3/3 | ok |
| html_contract *(spotlight)* | html | outline | 31 | 63.3 | 0.000077 | 0/3 | **DEGRADING** |
| csv_inventory | csv | faithful | 0 | 0.0 | 0.000000 | 3/3 | ok |
| csv_inventory | csv | structured | 0 | 0.0 | 0.000000 | 3/3 | ok |
| csv_inventory | csv | outline | 5 | 27.8 | 0.000013 | 0/3 | **DEGRADING** |
| json_config | json | faithful | 0 | 0.0 | 0.000000 | 3/3 | ok |
| json_config | json | outline | 4 | 25.0 | 0.000010 | 0/3 | **DEGRADING** |
| xml_order | xml | faithful | 0 | 0.0 | 0.000000 | 3/3 | ok |
| xml_order | xml | outline | 15 | 55.6 | 0.000037 | 0/3 | **DEGRADING** |
| text_memo | text | faithful | 0 | 0.0 | 0.000000 | 3/3 | ok |
| text_memo | text | outline | 24 | 82.8 | 0.000060 | 1/3 | **DEGRADING** |
| html_catalog | html | faithful | 4 | 16.7 | 0.000010 | 2/2 | ok |
| html_catalog | html | outline | 19 | 79.2 | 0.000048 | 1/2 | **DEGRADING** |

*(structured rows equal faithful for the structured-data formats — there is no
decorative boilerplate to drop; both shown for HTML/text.)*

## Aggregate + falsification

| tier | facts preserved (corpus) | verdict |
|---|:---:|---|
| faithful | 20/20 (100%) | answer-safe |
| structured | 20/20 (100%) | answer-safe |
| outline | 3/20 (15%) | **DEGRADING (lossy by design)** |

**The proof can come out negative, and does:** `outline` keeps only heading facts
(the 3 survivors are document headings); it drops body prose, table cells, and
fenced data — so for documents whose answers live there it is **not** answer-safe.
**The harness catches this** — a deliberately-degrading spotlight fixture
(`html_contract`, all facts in body/table) forces `outline` to 0/3, and a grader
hard-wired to 100% **fails** the `TestConversionProof_DetectsDegradation` spine.

## What this validates operationally

- **`faithful` / `structured` are answer-safe** to enable (`DistillPolicy` per
  workspace) — they reduce tokens (e.g. HTML −17%…−30% by tag-stripping) with **no
  answer-relevant fact loss** in this corpus.
- **`outline` is a "what-is-this-about" summary, not an answer-preserving tier** —
  its large savings come at measured fact loss; do not route answer-bearing
  document work through it.
- The vision-OCR path needs its own model-based eval before any claim — that is the **Vision-OCR fidelity benchmark** below.

---

# Vision-OCR fidelity benchmark — the higher-risk path

The model-free proof above **excludes** vision-OCR (a model reads pixels of a
scanned/text-less PDF) — the lossiest conversion and where savings-quality risk is
highest. This benchmark is the on-demand instrument that measures it.

**Framing: fidelity × cost, NOT savings × quality.** Vision-OCR **spends** tokens
(`TokensSaved=0`, `VisionTokensCost>0`). The question is not "does it save?" but
*"does the expensive Anthropic OCR recover answer-relevant facts well enough to
justify its cost?"*

Source: `internal/proxy/vision_benchmark_test.go` + fixtures under
`internal/distill/testdata/vision/`.

## Three execution modes (kept strictly separate)

1. **Keyless CI guards** — always run, deterministic, free, no key:
   - `TestVisionFixtures_AreTextLess` (`internal/distill/`) — pins that every
     committed fixture is **text-less** (routes to `NeedsVision`), so the benchmark
     measures real OCR, not text-layer reading. A fixture that grew a text layer
     would silently fake high fidelity; this guard prevents that.
   - `TestVisionGrader_OnRecordedTranscript` — validates the presence grader on a
     fixed recorded transcript. **Validates the grader, NOT the model.**
2. **Paid live benchmark** — `TestVisionFidelityBenchmark_Paid` — **skips cleanly**
   unless **both** `ANTHROPIC_API_KEY` and `LENS_VISION_BENCH=1` are set. When run,
   it OCRs each fixture **N times** (default 5, `LENS_VISION_BENCH_RUNS`) via real
   Anthropic and reports **worst-of-N** fact recovery paired with the honest
   `VisionTokensCost`. It spends tokens and is non-deterministic — **never a CI
   gate**.
3. **Throwaway generator** — `internal/distill/testdata/vision/gen_fixtures.go`
   (`//go:build ignore`) — renders text → image (basicfont) → image-only PDF.
   **Out of the module graph** (`x/image` never enters `go.mod`); committed for
   provenance + regeneration.

## How an operator runs it (the pre-enable gate)

Before enabling document OCR (`DistillPolicy` routing scanned PDFs) for a
customer, run the benchmark as the gate:

```
ANTHROPIC_API_KEY=sk-... LENS_VISION_BENCH=1 LENS_VISION_BENCH_RUNS=5 \
  go test ./internal/proxy/ -run TestVisionFidelityBenchmark_Paid -v
```

It emits a per-fixture + aggregate table: **worst-of-N fact recovery × average
`VisionTokensCost`**, headlined as *"recovers ≥X% of answer-relevant facts, worst
of N runs."* **The artifact is unpopulated in-repo by design** — it materializes
only when an operator runs the paid path (no tokens are spent speculatively).

## Bounds on the claim (read before trusting a number)

- **Clean synthetic renders are OPTIMISTIC.** The fixtures are crisp
  machine-rendered text; real scans (noise, skew, low DPI, handwriting) are harder.
  A good recovery rate here is a **ceiling**, not a field number — real-scan
  fidelity needs real-scan fixtures.
- **One-directional** (same as the model-free proof): it proves answer-relevant
  facts were **recovered**, NOT that nothing spurious/hallucinated was added.
- **Anthropic-only.** The live OCR path supports the Anthropic provider only
  (`visionProviderSupported`); the benchmark measures that path.
