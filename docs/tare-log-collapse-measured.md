# Tare — log collapse, measured

The third of the three phase-1 reductions W6.1 names ("JSON dedup, tree-sitter AST trimming, log
collapse"). `internal/tare/logs.go`, registered in the package conformance registry.

## It is lossless, and that was the design's call rather than this build's

`reports/tare-design-v1.html` says it four ways:

| where | what it says |
|---|---|
| §03 architecture diagram | `ContentRouter → JSON dedup · code AST (tree-sitter) · log-collapse [lossless first]` |
| §03 prose | "Lossless-first content routing. v1 ships only reductions that **cannot silently drop signal** an enterprise agent needs: JSON de-duplication, tree-sitter AST structural trimming (output stays syntactically valid), log collapse." |
| roadmap, go-live-aligned | "**Lossless** structural reduction (Phase 1) + the metering surface." |
| roadmap, phase 2+ | "the **aggressive lossy path** and reversible cache" |

A run of identical adjacent lines carries exactly one fact — the line, and how many times it
occurred — so collapsing it to one copy plus a count loses nothing. `tare.ExpandLog` puts the bytes
back, and the conformance harness compares the result **byte for byte**.

## There is no must-keep list here, and its absence is the point

W6.1 and an earlier revision of `conformance_test.go` treated the design's *"hardcoded must-keep for
numbers/paths/IDs"* as a phase-1 obligation this reducer would owe. Measured against the design
itself, that clause is a parenthetical glossing the word *extractive* inside the sourcing-table row
whose **Piece** is "Text model ModernBERT dual-head · extractive" and whose **Call** is "Vendor base
+ build LoRA" — the phase-2, gated ML model. §07 attributes it the same way: *"**the model** is
extractive (never rephrases)"*.

It is the mitigation for something that **chooses which tokens to discard**. This reducer discards
nothing, so every number, path and ID survives by construction —
`TestLog_NumbersPathsAndIDsAllSurviveTheRoundTrip` asserts it on a fixture carrying file paths, a
ULID, a user id, a sha, and an integer above 2^53. `docs/tare-phase1a-measured.md` made the same
argument for the JSON reducer's keep-rule: under a lossless transform the rule *cannot fail*.

The correction is recorded in `internal/tare/conformance_test.go`, merged as `ac5b350`.

## No minimum-run-length constant

"Collapse runs of 3 or more" would be a threshold nobody measured, and it is wrong in both
directions: a run of two 200-byte lines is worth collapsing and a run of five empty lines is not.
`worthCollapsing` compares the bytes saved against the bytes the marker costs, which derives the
minimum run length from the line itself. The document-level result is checked again before anything
is returned, so a per-run rule that ever disagreed with the whole-document one refuses instead of
booking a negative saving.

## ⚠ The measured saving, and it is nothing like the design's headline

Run over this project's own operational logs — 9 files, 1.62 MB, the supervisor and deploy logs of
the sessions that built this repo:

| file | bytes in | bytes out | saved |
|---|---:|---:|---:|
| `deploy.log` | 135,216 | 135,216 | **refused** — no run worth collapsing |
| `session-sup-A.log` | 433,043 | 404,820 | 6.5% |
| `session-sup-B.log` | 412,137 | 386,135 | 6.3% |
| `session-sup-C.log` | 383,445 | 356,092 | 7.1% |
| `session-sup-D.log` | 100,170 | 89,366 | 10.8% |
| `supervisor.log` | 148,250 | 148,250 | **refused** |
| `w34-providerctx-controls-4c7e.log` | 1,663 | 1,663 | **refused** |
| `w34-runctx-controls-4c7e.log` | 2,739 | 2,739 | **refused** |
| `watch.log` | 240 | 240 | **refused** |
| **total** | **1,616,903** | **1,524,521** | **5.7%** |

**Every reduced file round-tripped byte-exact.** Five of nine were refused outright.

The design's number for this content class is *"60–95% is JSON and logs"*. On this corpus,
adjacent-run collapse alone delivers **5.7%**, and more than half the files have no qualifying run
at all. The two are not in contradiction — 60–95% is plausible for machine-emitted logs with tight
retry loops, and these are human-paced session logs — but **nobody should quote 60–95% for logs on
the strength of this reducer**. W6.1's own note already says the pitch is attribution rather than a
headline percentage; this is the number behind that sentence.

⚠ **The corpus is not in this repository** (`~/talyvor-queue/*.log`), so there is no committed test
pinning these figures — the same defect that lets the design itself drift, recorded here rather than
worked around. What *is* committed is the round-trip property, which is what the saving's
auditability actually rests on.

## What it deliberately does not do

Non-adjacent duplicates are left alone. Collapsing them would need their positions recorded to be
reversible, which costs more than it saves; `TestLog_LeavesNonAdjacentDuplicatesAlone` pins it.

## Refusals

`wrong kind for this reducer` · `empty content` · `no run of repeated lines worth collapsing` ·
`the reduced form is not smaller than the input` · `content already contains the tare repeat marker`.

The last one is separate from "no repeats" on purpose: *this log has no repetition* is a fact about
the traffic, and *this log already contains our marker* is a fact about us — if it ever becomes
common, the marker is wrong.
