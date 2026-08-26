# Tare phase 1b — the structural code trimmer, measured

W6.1.2. **57.45%** over 400 real Go files in this repository (394 trimmed, 6 refused,
2 995 569 → 1 274 666 bytes).

## ⚠ Two things in the item are not both true, and this says which one was built

W6.1.2 says:

> KEEP imports, signatures and types; **drop bodies**.

and, four lines later:

> SAME RULES: **lossless**, refusable, wired to nothing, measured on real code.

**A dropped body is not recoverable from the output.** These cannot both hold. The behaviour
instruction is concrete and stated twice, so it is what was built — and this reducer is **named,
documented and tested as lossy**, rather than being filed under the other word because the word was
convenient. Phase 1a's JSON compressor *is* lossless (it round-trips); this one is not, and the two
must not be described together.

## ⚠ The elision announces itself — the design decision that matters most here

A body replaced by `{}` tells a reading agent **the function is empty**. That is not a smaller
truth, it is a **different and false one** — the same class of harm as the previous compressor
rewriting a prompt into `'to' versus 'to'`.

Every elided body carries a marker and a line count:

```go
func Run(ctx context.Context, c Config) (int, error) { /* tare: 11 lines elided */ }
```

An agent reading that knows to fetch the file. An agent reading `{}` does not know there is
anything to fetch. `TestGo_EveryElidedBodySaysSoInTheOutput` is the lock and control **I1** is what
proves the lock can fail.

## ⚠ Why Go's standard library and not tree-sitter — measured, not preferred

W6.1.2 specifies tree-sitter and says talyvor-code "ALREADY SHIPS A SEMANTIC INDEX WITH AST TOOLING
… Reuse or say why you cannot." Measured across both repos:

- **talyvor-code contains no tree-sitter at all** — no dependency, no binding, nothing to reuse. Its
  semantic index (`agent/internal/codebase/semindex_build.go`) is a SHA-256 content hash and a text
  chunker. `go/ast` appears in that repo only inside **test** guards.
- ⚠ **And tree-sitter could not ship here anyway.** Every Go tree-sitter binding is CGO, and this
  repo builds with `CGO_ENABLED=0` in **both** the CI build step (`.github/workflows/ci.yaml`) and
  the `Dockerfile`. A CGO dependency would not compile — so a tree-sitter trimmer would run
  **nowhere**, which is the same trap as building Tare into an Envoy that has never run.

`go/parser` is in the standard library: no new dependency, no CGO, and the output's validity is
checkable directly.

**The cost of that choice, stated: this covers Go only.** A multi-language trimmer needs either
pure-Go parsers per language, a WASM tree-sitter runtime, or enabling CGO in the build. That is a
decision, not an oversight, and it is not taken here.

## Why it splices source instead of printing an AST

`go/printer` would re-emit the whole file — reformatting everything kept and moving comments. This
locates each body's byte range and splices, so **every kept region is byte-identical**: imports,
types, signatures and their doc comments come through exactly as written, and a reader diffing the
reduced form against the original sees only the elisions.
`TestGo_KeptRegionsAreByteIdenticalNotReformatted` pins it with deliberately odd spacing.

## The output is re-parsed before it is returned

W6.1.2: *"THE OUTPUT MUST STAY SYNTACTICALLY VALID — assert it re-parses, per language, or an agent
receives a file it cannot reason about."*

The trimmer re-parses its own output and **refuses** if it does not parse. A test proves validity
for the inputs a test has; this turns any shape nobody anticipated into a refusal rather than a
broken file. Control **I3** removes it and the trimmer starts emitting unparseable Go.

## Refusals

`wrong kind` · `empty` · `content does not parse as Go` · `no function bodies to elide` · **`every
function body is smaller than the elision marker`** · `not smaller` · `failed to re-parse`.

The fifth is distinct from the fourth on purpose: "there are no bodies" and "every body is shorter
than the marker that would replace it" are different facts about a file, and a refusal nobody can
tell apart is one nobody can act on. Trimming `{ x() }` would **grow** the file, so it is left alone
— per body, not just per file.

## Controls

`w612-gotrim-controls-k7v3.py` — **5/5 CAUGHT**.

⚠ **A sixth control is recorded and NOT run, because it cannot fire.** I5 replaced the brace-offset
sanity check with `false` and **nothing went red**: for any input that parsed, `go/parser` always
reports sane offsets, so that branch is unreachable. Deleting the control would hide that; scoring
it as a failure would be false. It is written down in the harness and in `gocode.go`, which now
describes that check as **defence-in-depth with no reachable trigger** rather than as a tested
guard — the same handling the unreachable savepoint rollback got in W4.6.1 step 1.

## Measured

```
400 Go files in this repository (394 trimmed, 6 refused): 2995569 -> 1274666 bytes (57.45%)
```

⚠ **Population boundary.** This is *this repository's* Go — a dense, heavily-commented Go codebase.
The number will differ on code with shorter functions or fewer doc comments, and it says nothing
about other languages. As in phase 1a: **no captured agent traffic exists**, so this is the
transform's behaviour on real code, not a forecast of production saving.

## Wired to nothing

No call site on the serve path, as the item requires.
